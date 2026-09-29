package eventlog

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeLines(t *testing.T, path string, mtime time.Time, lines ...string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !mtime.IsZero() {
		_ = os.Chtimes(path, mtime, mtime)
	}
}

func rec(ts time.Time, level, src, event string, extra string) string {
	s := `{"ts":"` + ts.Format(TimeLayout) + `","level":"` + level + `","msg":"` + event + `","src":"` + src + `","event":"` + event + `"`
	if extra != "" {
		s += "," + extra
	}
	return s + "}"
}

func TestReadDirsMergesAcrossFilesAndSources(t *testing.T) {
	root := t.TempDir()
	eng, dmn := filepath.Join(root, "user"), filepath.Join(root, "system")
	at := func(m int) time.Time { return t0.Add(time.Duration(m) * time.Minute) }
	// A rotated engine file, the active engine file (with a corrupt line
	// and a torn tail), and the daemon's file interleaved in time.
	writeLines(t, filepath.Join(eng, "engine-20260928T212500.jsonl"), at(2),
		rec(at(0), "INFO", "engine", "session.start", `"version":"dev"`),
		rec(at(2), "INFO", "engine", "sample", `"thermal":"nominal"`))
	writeLines(t, filepath.Join(eng, "engine.jsonl"), at(10),
		rec(at(3), "INFO", "engine", "thermal.transition", `"from":"NOMINAL","to":"FAIR","cpu_c":71`),
		`not json at all`,
		rec(at(5), "WARN", "engine", "lease.renew_fail", `"err":"i/o timeout"`),
		rec(at(9), "DEBUG", "engine", "thermal.note", ""),
		`{"ts":"2026-09-28T21:34:00.000-04:00","level":"INFO","msg":"torn`)
	writeLines(t, filepath.Join(dmn, "backpackd.jsonl"), at(10),
		rec(at(1), "INFO", "daemon", "lease.acquire", `"uid":501`),
		rec(at(3), "INFO", "daemon", "flag.set", `"value":true,"verified":true`),
		rec(at(6), "INFO", "daemon", "lease.release", ""))
	// An old rotated file outside the window is skipped by mtime.
	writeLines(t, filepath.Join(dmn, "backpackd-20260101T000000.jsonl"), at(-10000),
		rec(at(-10000), "INFO", "daemon", "daemon.start", ""))

	evs, st, err := ReadDirs([]string{eng, dmn, filepath.Join(root, "missing")}, Filter{Since: at(-60)})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, ev := range evs {
		got = append(got, ev.Src+":"+ev.Name)
	}
	want := []string{
		"engine:session.start", "daemon:lease.acquire", "engine:sample",
		"engine:thermal.transition", "daemon:flag.set", "engine:lease.renew_fail", "daemon:lease.release",
	}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("order:\n got %v\nwant %v", got, want)
	}
	if st.Files != 3 || st.Corrupt != 2 {
		t.Fatalf("stats = %+v, want 3 files (old one skipped), 2 corrupt", st)
	}

	// Filters: level, src, window.
	evs, _, _ = ReadDirs([]string{eng, dmn}, Filter{MinLevel: slog.LevelWarn})
	if len(evs) != 1 || evs[0].Name != "lease.renew_fail" {
		t.Fatalf("warn filter: %v", evs)
	}
	evs, _, _ = ReadDirs([]string{eng, dmn}, Filter{Srcs: []string{"daemon"}, Since: at(2), Until: at(6)})
	if len(evs) != 1 || evs[0].Name != "flag.set" {
		t.Fatalf("src+window filter: %v", evs)
	}
	evs, _, _ = ReadDirs([]string{eng}, Filter{MinLevel: slog.LevelDebug, Since: at(8)})
	if len(evs) != 1 || evs[0].Name != "thermal.note" {
		t.Fatalf("debug filter: %v", evs)
	}
}

func TestParseLineKeepsFieldOrderAndRaw(t *testing.T) {
	l := rec(t0, "INFO", "engine", "sample", `"thermal":"fair","cpu_c":71.4,"load":1.5,"battery":80,"lid":"closed","online":true,"iface":"en10"`)
	ev, err := ParseLine([]byte(l))
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, a := range ev.Attrs {
		keys = append(keys, a.Key)
	}
	if strings.Join(keys, ",") != "thermal,cpu_c,load,battery,lid,online,iface" {
		t.Fatalf("keys = %v", keys)
	}
	b, _ := json.Marshal(ev)
	if string(b) != l {
		t.Fatalf("MarshalJSON = %s, want raw line", b)
	}
	for _, bad := range []string{`{"level":"INFO","msg":"no ts"}`, `{"ts":"yesterday"}`, `[1,2]`, `{"ts":"2026-09-28T21:24:00Z"} trailing`} {
		if _, err := ParseLine([]byte(bad)); err == nil {
			t.Errorf("ParseLine(%s) accepted", bad)
		}
	}
}

func TestFormat(t *testing.T) {
	loc := time.FixedZone("EDT", -4*3600)
	ts := time.Date(2026, 9, 28, 21, 24, 3, 0, loc)
	cases := []struct{ line, want string }{
		{rec(ts, "INFO", "engine", "thermal.transition", `"from":"NOMINAL","to":"FAIR","thermal":"fair","cpu_c":71.3`),
			"21:24:03 engine  thermal.transition  NOMINAL→FAIR thermal=fair cpu=71°C"},
		{rec(ts, "WARN", "engine", "lease.renew_fail", `"err":"read: i/o timeout"`),
			`21:24:03 engine  lease.renew_fail    WARN err="read: i/o timeout"`},
		{rec(ts, "INFO", "engine", "sample", `"battery":80,"ac":false,"lid":"closed","order":["iPhone USB","Wi-Fi"]`),
			`21:24:03 engine  sample              battery=80% ac=false lid=closed order=["iPhone USB","Wi-Fi"]`},
		{`{"ts":"2026-09-28T21:24:03.000-04:00","level":"ERROR","msg":"engine panic","src":"engine","panic":"boom"}`,
			"21:24:03 engine  engine panic        ERROR panic=boom"},
		{rec(ts, "INFO", "engine", "power.transition", `"from":"AC","to":"battery","battery":97,"note":""`),
			`21:24:03 engine  power.transition    AC→battery battery=97% note=""`},
	}
	for _, c := range cases {
		ev, err := ParseLine([]byte(c.line))
		if err != nil {
			t.Fatal(err)
		}
		if got := Format(ev, loc); got != c.want {
			t.Errorf("Format:\n got %q\nwant %q", got, c.want)
		}
	}
	// A synthesized macOS event shows only its summary and marshals to a
	// JSONL-shaped object.
	evs, _ := ParsePmsetLog(strings.NewReader("2026-09-28 17:23:51 -0400 Sleep               \tEntering Sleep state due to 'Clamshell Sleep':TCPKeepAlive=active Using AC (Charge:100%) 223 secs  \n"), time.Time{}, time.Time{})
	if len(evs) != 1 {
		t.Fatalf("pmset events: %v", evs)
	}
	if got, want := Format(evs[0], loc), "17:23:51 macos   pm.sleep            WARN Sleep due to 'Clamshell Sleep' · AC 100% · 223s"; got != want {
		t.Errorf("macos Format:\n got %q\nwant %q", got, want)
	}
	b, _ := json.Marshal(evs[0])
	back, err := ParseLine(b)
	if err != nil || back.Src != SrcMacOS || back.Name != EventPMSleep || !back.Time.Equal(evs[0].Time) {
		t.Fatalf("synthesized JSON %s did not round-trip: %v %+v", b, err, back)
	}
}
