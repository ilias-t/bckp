package eventlog

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestFollowerTailsAcrossRotation(t *testing.T) {
	root := t.TempDir()
	eng, dmn := filepath.Join(root, "user"), filepath.Join(root, "system")
	clk := &fakeNow{t: t0}
	w, err := NewWriter(eng, "engine", Options{MaxFileBytes: 300, Now: clk.Now})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	write := func(ts time.Time, event string) {
		t.Helper()
		if _, err := w.Write([]byte(rec(ts, "INFO", "engine", event, `"pad":"xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"`) + "\n")); err != nil {
			t.Fatal(err)
		}
	}
	write(t0, "old.before_follow")

	fl := NewFollower([]string{eng, dmn}, Filter{})
	defer fl.Close()
	if evs, err := fl.Poll(); err != nil || len(evs) != 0 {
		t.Fatalf("first poll: %v %v (history is not replayed)", evs, err)
	}

	// Enough writes to rotate at least once between polls.
	write(t0.Add(1*time.Second), "a")
	write(t0.Add(2*time.Second), "b")
	write(t0.Add(3*time.Second), "c")
	write(t0.Add(4*time.Second), "d")
	// The daemon's log appears later: read from its start.
	writeLines(t, filepath.Join(dmn, "backpackd.jsonl"), time.Time{}, rec(t0.Add(2500*time.Millisecond), "INFO", "daemon", "lease.acquire", ""))
	// A torn line is held until its newline arrives.
	f, _ := os.OpenFile(filepath.Join(dmn, "backpackd.jsonl"), os.O_APPEND|os.O_WRONLY, 0)
	partial := rec(t0.Add(5*time.Second), "INFO", "daemon", "lease.release", "")
	_, _ = f.WriteString(partial[:20])

	evs, err := fl.Poll()
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, ev := range evs {
		got = append(got, ev.Name)
	}
	if want := "a b lease.acquire c d"; join(got) != want {
		t.Fatalf("poll 2 = %q, want %q", join(got), want)
	}
	if n := len(names(t, eng)); n < 2 {
		t.Fatalf("expected a rotation, files: %v", names(t, eng))
	}

	_, _ = f.WriteString(partial[20:] + "\n")
	f.Close()
	write(t0.Add(6*time.Second), "e")
	evs, _ = fl.Poll()
	got = nil
	for _, ev := range evs {
		got = append(got, ev.Name)
	}
	if want := "lease.release e"; join(got) != want {
		t.Fatalf("poll 3 = %q, want %q", join(got), want)
	}

	// Level filtering applies to followed events too.
	fl2 := NewFollower([]string{eng}, Filter{MinLevel: slog.LevelWarn})
	defer fl2.Close()
	_, _ = fl2.Poll()
	write(t0.Add(7*time.Second), "info.only")
	if evs, _ := fl2.Poll(); len(evs) != 0 {
		t.Fatalf("warn follower got %v", evs)
	}
}

func join(s []string) string {
	out := ""
	for i, x := range s {
		if i > 0 {
			out += " "
		}
		out += x
	}
	return out
}
