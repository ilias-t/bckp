package eventlog

import (
	"bytes"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// fakeNow is a settable clock for rotation stamps and age purging.
type fakeNow struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeNow) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeNow) Add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

var t0 = time.Date(2026, 9, 28, 21, 24, 0, 0, time.Local)

func line(i, size int) []byte {
	s := fmt.Sprintf(`{"ts":"%s","level":"INFO","msg":"x","src":"engine","n":%d,"pad":"`, t0.Format(TimeLayout), i)
	pad := size - len(s) - 3
	return []byte(s + strings.Repeat("p", max(pad, 0)) + "\"}\n")
}

func names(t *testing.T, dir string) []string {
	t.Helper()
	files, err := Files(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, f := range files {
		out = append(out, filepath.Base(f.Path))
	}
	return out
}

func totalSize(t *testing.T, dir string) int64 {
	t.Helper()
	files, _ := Files(dir)
	var n int64
	for _, f := range files {
		n += f.Size
	}
	return n
}

func TestWriterRotatesBySize(t *testing.T) {
	dir := t.TempDir()
	clk := &fakeNow{t: t0}
	w, err := NewWriter(dir, "engine", Options{MaxFileBytes: 1000, MaxTotalBytes: 1 << 20, Now: clk.Now})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	for i := range 25 { // 25 x 100 bytes: 10 per file
		if _, err := w.Write(line(i, 100)); err != nil {
			t.Fatal(err)
		}
		clk.Add(time.Second)
	}
	got := names(t, dir)
	want := []string{"engine-20260928T212410.jsonl", "engine-20260928T212420.jsonl", "engine.jsonl"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("files = %v, want %v", got, want)
	}
	for _, f := range got {
		fi, _ := os.Stat(filepath.Join(dir, f))
		if fi.Size() > 1000 {
			t.Errorf("%s is %d bytes, over the 1000 cap", f, fi.Size())
		}
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("%s mode %v, want 0600", f, fi.Mode().Perm())
		}
	}
	evs, st, err := ReadDirs([]string{dir}, Filter{})
	if err != nil || len(evs) != 25 || st.Corrupt != 0 {
		t.Fatalf("read back %d events (stats %+v, err %v), want 25", len(evs), st, err)
	}
}

func TestWriterNameCollisions(t *testing.T) {
	dir := t.TempDir()
	clk := &fakeNow{t: t0} // never advances: every rotation in the same second
	w, err := NewWriter(dir, "engine", Options{MaxFileBytes: 100, Now: clk.Now})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	for i := range 4 {
		if _, err := w.Write(line(i, 100)); err != nil {
			t.Fatal(err)
		}
	}
	got := strings.Join(names(t, dir), ",")
	for _, want := range []string{"engine-20260928T212400.jsonl", "engine-20260928T212400-2.jsonl", "engine-20260928T212400-3.jsonl", "engine.jsonl"} {
		if !strings.Contains(got, want) {
			t.Errorf("files %s: missing %s", got, want)
		}
	}
}

func TestWriterTotalCapPurgesOldest(t *testing.T) {
	dir := t.TempDir()
	clk := &fakeNow{t: t0}
	w, err := NewWriter(dir, "engine", Options{MaxFileBytes: 1000, MaxTotalBytes: 3000, Now: clk.Now})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	for i := range 100 {
		if _, err := w.Write(line(i, 100)); err != nil {
			t.Fatal(err)
		}
		clk.Add(time.Second)
	}
	if n := totalSize(t, dir); n > 3000 {
		t.Fatalf("total %d bytes, over the 3000 cap", n)
	}
	evs, _, _ := ReadDirs([]string{dir}, Filter{})
	if len(evs) < 20 {
		t.Fatalf("kept %d events; the newest ~2-3 files should survive", len(evs))
	}
	// The newest records survive, the oldest are gone.
	last, _ := evs[len(evs)-1].Get("n")
	first, _ := evs[0].Get("n")
	if plain(last) != "99" || plain(first) == "0" {
		t.Fatalf("kept n=%v..%v, want the newest ending at 99", first, last)
	}
}

func TestWriterAgePurge(t *testing.T) {
	dir := t.TempDir()
	clk := &fakeNow{t: t0}
	w, err := NewWriter(dir, "engine", Options{MaxFileBytes: 200, MaxAge: 24 * time.Hour, Now: clk.Now})
	if err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		_, _ = w.Write(line(i, 150)) // each write after the first rotates
		clk.Add(time.Hour)
	}
	before := names(t, dir)
	if len(before) != 3 {
		t.Fatalf("files = %v, want 2 rotated + active", before)
	}
	clk.Add(48 * time.Hour)
	_, _ = w.Write(line(9, 150)) // rotates and purges
	w.Close()
	got := names(t, dir)
	// Only the file rotated just now survives next to the active one.
	if len(got) != 2 || !strings.Contains(strings.Join(got, ","), "engine.jsonl") || !strings.Contains(strings.Join(got, ","), "engine-20261001T002400.jsonl") {
		t.Fatalf("files after age purge = %v", got)
	}
	// Other logs' files in the same dir are never touched.
	other := filepath.Join(dir, "backpackd-20200101T000000.jsonl")
	if err := os.WriteFile(other, line(0, 50), 0o644); err != nil {
		t.Fatal(err)
	}
	old := t0.Add(-100 * 24 * time.Hour)
	_ = os.Chtimes(other, old, old)
	w2, err := NewWriter(dir, "engine", Options{MaxAge: time.Hour, Now: clk.Now})
	if err != nil {
		t.Fatal(err)
	}
	w2.Close()
	if _, err := os.Stat(other); err != nil {
		t.Fatalf("purge removed another log's file: %v", err)
	}
}

func TestWriterConcurrent(t *testing.T) {
	dir := t.TempDir()
	log, c, err := Open(dir, "engine", Options{MaxFileBytes: 4096, MaxTotalBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	const writers, each = 8, 200
	var wg sync.WaitGroup
	for g := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range each {
				Log(log, slog.LevelInfo, "test.concurrent", "g", g, "i", i, "pad", strings.Repeat("x", 40))
			}
		}()
	}
	wg.Wait()
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	evs, st, err := ReadDirs([]string{dir}, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if st.Corrupt != 0 || len(evs) != writers*each {
		t.Fatalf("read %d events, %d corrupt; want %d, 0", len(evs), st.Corrupt, writers*each)
	}
	seen := map[string]bool{}
	for _, ev := range evs {
		g, _ := ev.Get("g")
		i, _ := ev.Get("i")
		seen[plain(g)+"/"+plain(i)] = true
	}
	if len(seen) != writers*each {
		t.Fatalf("%d distinct records, want %d", len(seen), writers*each)
	}
	if len(names(t, dir)) < 2 {
		t.Fatal("expected rotation during the concurrent writes")
	}
}

func TestWriterTerminatesTornLine(t *testing.T) {
	dir := t.TempDir()
	good := line(1, 80)
	torn := line(2, 80)[:40] // crash mid-write: no newline
	if err := os.WriteFile(filepath.Join(dir, "engine.jsonl"), append(good, torn...), 0o600); err != nil {
		t.Fatal(err)
	}
	w, err := NewWriter(dir, "engine", Options{})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.Write(line(3, 80))
	w.Close()
	b, _ := os.ReadFile(filepath.Join(dir, "engine.jsonl"))
	lines := bytes.Split(bytes.TrimSuffix(b, []byte("\n")), []byte("\n"))
	if len(lines) != 3 || !bytes.Equal(lines[1], torn) {
		t.Fatalf("file lines = %q; want good, torn, new on separate lines", lines)
	}
	evs, st, _ := ReadDirs([]string{dir}, Filter{})
	if len(evs) != 2 || st.Corrupt != 1 {
		t.Fatalf("read %d events with %d corrupt; want 2 and 1", len(evs), st.Corrupt)
	}
}

func TestOpenModesAndRedaction(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "logs")
	log, c, err := Open(dir, DaemonName, Options{Src: SrcDaemon, DirMode: 0o755, FileMode: 0o644, Redact: []string{"s3cr3t-topic", ""}})
	if err != nil {
		t.Fatal(err)
	}
	Log(log, slog.LevelWarn, "notify.failed", "title", "hot", "err", fmt.Errorf(`Post "https://ntfy.sh/s3cr3t-topic": timeout`),
		"list", []string{"a s3cr3t-topic"})
	log.Info("free text s3cr3t-topic")
	c.Close()
	di, _ := os.Stat(dir)
	fi, _ := os.Stat(filepath.Join(dir, "backpackd.jsonl"))
	if di.Mode().Perm() != 0o755 || fi.Mode().Perm() != 0o644 {
		t.Fatalf("modes dir %v file %v, want 0755/0644", di.Mode().Perm(), fi.Mode().Perm())
	}
	b, _ := os.ReadFile(filepath.Join(dir, "backpackd.jsonl"))
	if bytes.Contains(b, []byte("s3cr3t")) {
		t.Fatalf("secret leaked: %s", b)
	}
	evs, _, _ := ReadDirs([]string{dir}, Filter{})
	if len(evs) != 2 {
		t.Fatalf("got %d events", len(evs))
	}
	ev := evs[0]
	if ev.Src != SrcDaemon || ev.Name != "notify.failed" || ev.Level != slog.LevelWarn {
		t.Fatalf("event = %+v", ev)
	}
	if e, _ := ev.Get("err"); !strings.Contains(plain(e), "<redacted>") {
		t.Fatalf("err = %v", e)
	}
	if !strings.Contains(string(ev.Raw), `"ts":"`) || strings.Contains(string(ev.Raw), `"time":`) {
		t.Fatalf("raw line should use ts: %s", ev.Raw)
	}
	if evs[1].Name != "free text <redacted>" {
		t.Fatalf("msg-only event name = %q", evs[1].Name)
	}
}

func TestWithConfig(t *testing.T) {
	o := Options{}.WithConfig(10, 48*time.Hour).withDefaults()
	if o.MaxTotalBytes != 10<<20 || o.MaxAge != 48*time.Hour || o.MaxFileBytes != DefaultMaxFileBytes {
		t.Fatalf("WithConfig = %+v", o)
	}
	o = Options{}.WithConfig(0, 0).withDefaults()
	if o.MaxTotalBytes != DefaultMaxTotalBytes || o.MaxAge != DefaultMaxAge {
		t.Fatalf("WithConfig defaults = %+v", o)
	}
	o = Options{}.WithConfig(1, 0).withDefaults()
	if o.MaxFileBytes > o.MaxTotalBytes {
		t.Fatalf("file cap %d above total %d", o.MaxFileBytes, o.MaxTotalBytes)
	}
}

func TestWriterRefusesSymlinksAndNonRegularFiles(t *testing.T) {
	// A symlinked log file is not followed: the target is untouched.
	dir := t.TempDir()
	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, []byte("keep\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "engine"+Ext)); err != nil {
		t.Fatal(err)
	}
	if _, err := NewWriter(dir, "engine", Options{FileMode: 0o644}); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("symlinked file: err = %v", err)
	}
	if b, _ := os.ReadFile(target); string(b) != "keep\n" {
		t.Fatalf("target changed: %q", b)
	}
	if fi, _ := os.Stat(target); fi.Mode().Perm() != 0o600 {
		t.Fatalf("target mode changed: %v", fi.Mode())
	}

	// A symlinked log dir is refused before it is chmodded.
	realDir := t.TempDir()
	if err := os.Chmod(realDir, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "logs")
	if err := os.Symlink(realDir, link); err != nil {
		t.Fatal(err)
	}
	if _, err := NewWriter(link, "engine", Options{DirMode: 0o755}); err == nil {
		t.Fatal("symlinked dir accepted")
	}
	if fi, _ := os.Stat(realDir); fi.Mode().Perm() != 0o700 {
		t.Fatalf("symlinked dir chmodded: %v", fi.Mode())
	}

	// Anything but a regular file (here a FIFO) is refused.
	dir = t.TempDir()
	if err := syscall.Mkfifo(filepath.Join(dir, "engine"+Ext), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewWriter(dir, "engine", Options{}); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("FIFO: err = %v", err)
	}
}
