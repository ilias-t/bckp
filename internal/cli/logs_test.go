package cli

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ilias-t/bckp/internal/contracts"
	"github.com/ilias-t/bckp/internal/eventlog"
	"github.com/ilias-t/bckp/internal/paths"
)

var pmsetKey = contracts.CommandLine(paths.BinPmset, pmsetLogArgs...)

// at is a time on testNow's day (21:10 local).
func at(hms string) time.Time {
	t, err := time.ParseInLocation("2006-01-02 15:04:05", "2026-09-28 "+hms, time.Local)
	if err != nil {
		panic(err)
	}
	return t
}

func jl(ts time.Time, level, src, event, extra string) string {
	s := `{"ts":"` + ts.Format(eventlog.TimeLayout) + `","level":"` + level + `","msg":"` + event + `","src":"` + src + `","event":"` + event + `"`
	if extra != "" {
		s += "," + extra
	}
	return s + "}\n"
}

func pmLine(ts time.Time, typ, msg string) string {
	return ts.Format("2006-01-02 15:04:05 -0700") + " " + typ + strings.Repeat(" ", max(1, 20-len(typ))) + "\t" + msg + "  \n"
}

// logsFixture writes an engine log (one rotated file, one active with a
// torn tail), a daemon log, and a pmset log answer.
func logsFixture(t *testing.T, te *testEnv) {
	t.Helper()
	eng := te.env.user.LogDir
	dmn := te.env.sys(paths.DaemonLogDir)
	for _, d := range []string{eng, dmn} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(path string, mtime time.Time, content string) {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		_ = os.Chtimes(path, mtime, mtime)
	}
	write(filepath.Join(eng, "engine-20260928T201500.jsonl"), at("20:15:00"),
		jl(at("17:00:00"), "INFO", "engine", "sample", `"thermal":"nominal"`)+
			jl(at("20:10:00"), "INFO", "engine", "session.start", `"version":"dev","pid":4242`)+
			jl(at("20:10:01"), "INFO", "engine", "lease.acquire", `"lease":"ab12","ttl":"1m30s"`))
	write(filepath.Join(eng, "engine.jsonl"), at("21:05:00"),
		jl(at("20:30:00"), "INFO", "engine", "sample", `"fsm":"NOMINAL","thermal":"fair","cpu_c":64.2,"load":2.1,"battery":100,"ac":true,"lid":"closed","sleep_disabled":true,"net":"en10","online":true`)+
			jl(at("20:31:15"), "INFO", "engine", "thermal.transition", `"from":"NOMINAL","to":"FAIR","thermal":"fair","cpu_c":71.4`)+
			jl(at("20:40:00"), "DEBUG", "engine", "net.note", "")+
			jl(at("20:52:30"), "WARN", "engine", "suspected_sleep", `"gap":"6m15s","expected":"15s"`)+
			jl(at("20:53:00"), "ERROR", "engine", "notify.failed", `"title":"Mac is HOT","priority":4,"err":"Post \"https://ntfy.sh/topsecret-xyz\": timeout"`)+
			`{"ts":"2026-09-28T21:04:00.000`)
	write(filepath.Join(dmn, "backpackd.jsonl"), at("21:00:00"),
		jl(at("20:10:01"), "INFO", "daemon", "lease.acquire", `"lease":"ab12","uid":501`)+
			jl(at("20:10:01"), "INFO", "daemon", "flag.set", `"value":true,"verified":true`))
	te.ex.On(pmsetKey,
		pmLine(at("20:46:10"), "Assertions", "PID 1(x) Released PreventUserIdleSystemSleep \"Sleep things\"")+
			pmLine(at("20:46:15"), "Sleep", "Entering Sleep state due to 'Clamshell Sleep':TCPKeepAlive=active Using AC (Charge:100%) 375 secs")+
			pmLine(at("20:46:16"), "Wake Requests", "[process=dasd request=SleepService]")+
			pmLine(at("20:52:30"), "Wake", "Wake from Deep Idle [CDNVA] : due to smc.sysState.Wake(0x70070000) lid/UserActivity Assertion Using AC (Charge:100%) 90 secs"))
}

func TestLogsTimeline(t *testing.T) {
	te := newTestEnv(t)
	logsFixture(t, te)
	if code := te.run("logs"); code != exitOK {
		t.Fatalf("exit %d: %s", code, te.errb)
	}
	want := `--- Mon 2026-09-28 ---
20:10:00 engine  session.start       version=dev pid=4242
20:10:01 engine  lease.acquire       lease=ab12 ttl=1m30s
20:10:01 daemon  lease.acquire       lease=ab12 uid=501
20:10:01 daemon  flag.set            value=true verified=true
20:30:00 engine  sample              fsm=NOMINAL thermal=fair cpu=64°C load=2.1 battery=100% ac=true lid=closed sleep_disabled=true net=en10 online=true
20:31:15 engine  thermal.transition  NOMINAL→FAIR thermal=fair cpu=71°C
20:46:15 macos   pm.sleep            WARN Sleep due to 'Clamshell Sleep' · AC 100% · 375s
20:52:30 engine  suspected_sleep     WARN gap=6m15s expected=15s
20:52:30 macos   pm.wake             Wake from Deep Idle: smc.sysState.Wake(0x70070000) lid/UserActivity Assertion · AC 100% · 90s
20:53:00 engine  notify.failed       ERROR title="Mac is HOT" priority=4 err="Post \"https://ntfy.sh/topsecret-xyz\": timeout"
`
	if got := te.out.String(); got != want {
		t.Fatalf("timeline:\n%s\nwant:\n%s", got, want)
	}
	if !strings.Contains(te.errb.String(), "skipped 1 unreadable log line") {
		t.Fatalf("stderr = %q", te.errb)
	}
}

func TestLogsFilters(t *testing.T) {
	cases := []struct {
		args []string
		want []string // event names in order
	}{
		{[]string{"--level", "warn"}, []string{"pm.sleep", "suspected_sleep", "notify.failed"}},
		{[]string{"--src", "daemon"}, []string{"lease.acquire", "flag.set"}},
		{[]string{"--src", "engine,macos", "--since", "20:45"}, []string{"pm.sleep", "suspected_sleep", "pm.wake", "notify.failed"}},
		{[]string{"--since", "30m"}, []string{"pm.sleep", "suspected_sleep", "pm.wake", "notify.failed"}},
		{[]string{"--since", "1d", "--src", "engine", "--level", "debug"}, []string{"sample", "session.start", "lease.acquire", "sample", "thermal.transition", "net.note", "suspected_sleep", "notify.failed"}},
	}
	for _, c := range cases {
		t.Run(strings.Join(c.args, " "), func(t *testing.T) {
			te := newTestEnv(t)
			logsFixture(t, te)
			if code := te.run(append([]string{"logs", "--json"}, c.args...)...); code != exitOK {
				t.Fatalf("exit %d: %s", code, te.errb)
			}
			var got []string
			for _, l := range strings.Split(strings.TrimSpace(te.out.String()), "\n") {
				if l == "" {
					continue
				}
				ev, err := eventlog.ParseLine([]byte(l))
				if err != nil {
					t.Fatalf("--json line %q: %v", l, err)
				}
				got = append(got, ev.Name)
			}
			if strings.Join(got, " ") != strings.Join(c.want, " ") {
				t.Fatalf("got %v\nwant %v", got, c.want)
			}
			if !slicesContainsSrc(c.args, "macos") && slicesContainsSrc(c.args, "--src") && strings.Contains(strings.Join(te.ex.Calls(), "\n"), "pmset -g log") {
				t.Fatal("pmset ran although macos was not requested")
			}
		})
	}
}

func slicesContainsSrc(args []string, s string) bool {
	return strings.Contains(strings.Join(args, " "), s)
}

func TestLogsUsageErrors(t *testing.T) {
	for _, args := range [][]string{
		{"logs", "--since", "yesterday"},
		{"logs", "--level", "loud"},
		{"logs", "--src", "kernel"},
		{"logs", "2h"},
		{"logs", "--bundle", "--follow"},
	} {
		te := newTestEnv(t)
		if code := te.run(args...); code != exitUsage {
			t.Errorf("%v: exit %d, want %d (%s)", args, code, exitUsage, te.errb)
		}
	}
}

func TestLogsPmsetFailureStillShowsLogs(t *testing.T) {
	te := newTestEnv(t)
	logsFixture(t, te)
	te.ex = nil
	te.env.exec = failingExec{}
	if code := te.run("logs", "--src", "daemon,macos"); code != exitOK {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(te.errb.String(), "macOS power log unavailable") || !strings.Contains(te.out.String(), "flag.set") {
		t.Fatalf("stdout %q stderr %q", te.out, te.errb)
	}
}

type failingExec struct{}

func (failingExec) Run(context.Context, string, ...string) (contracts.ExecResult, error) {
	return contracts.ExecResult{ExitCode: 1}, errors.New("pmset: boom")
}

func TestLogsEmpty(t *testing.T) {
	te := newTestEnv(t)
	te.ex.On(pmsetKey, "")
	if code := te.run("logs"); code != exitOK {
		t.Fatalf("exit %d", code)
	}
	if te.out.Len() != 0 || !strings.Contains(te.errb.String(), "no events since") {
		t.Fatalf("stdout %q stderr %q", te.out, te.errb)
	}
}

func TestLogsFollow(t *testing.T) {
	te := newTestEnv(t)
	logsFixture(t, te)
	ctx, cancel := context.WithCancel(context.Background())
	te.env.signalContext = func(context.Context) (context.Context, context.CancelFunc) { return ctx, cancel }
	active := filepath.Join(te.env.user.LogDir, "engine.jsonl")
	sleeps := 0
	te.env.sleep = func(time.Duration) {
		sleeps++
		switch sleeps {
		case 1:
			f, _ := os.OpenFile(active, os.O_APPEND|os.O_WRONLY, 0)
			// Completes the torn line (it becomes a corrupt line) and adds
			// a new event.
			_, _ = f.WriteString("\n" + jl(at("21:10:05"), "WARN", "engine", "lease.renew_fail", `"err":"timeout"`))
			f.Close()
		case 2:
			cancel()
		}
	}
	if code := te.run("logs", "--follow", "--src", "engine", "--since", "5m"); code != exitOK {
		t.Fatalf("exit %d: %s", code, te.errb)
	}
	want := "--- Mon 2026-09-28 ---\n21:10:05 engine  lease.renew_fail    WARN err=timeout\n"
	if te.out.String() != want {
		t.Fatalf("follow output %q, want %q", te.out, want)
	}
}

func TestLogsBundle(t *testing.T) {
	te := newTestEnv(t)
	logsFixture(t, te)
	te.env.environ = []string{"BACKPACK_NOTIFY_NTFY_TOPIC=topsecret-xyz"}
	if err := os.WriteFile(te.env.user.EngineLog, []byte("=== starting ===\npanic: topsecret-xyz exploded\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(te.env.sys(paths.DaemonLogDir), "backpackd.log"), []byte("panic: boom\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if code := te.run("logs", "--since", "2h", "--bundle", dir); code != exitOK {
		t.Fatalf("exit %d: %s", code, te.errb)
	}
	path := filepath.Join(dir, "backpack-logs-20260928T211000.tar.gz")
	if !strings.Contains(te.out.String(), "wrote "+path) {
		t.Fatalf("stdout %q", te.out)
	}
	files := readTarGz(t, path)
	for _, name := range []string{
		"MANIFEST.txt", "engine/engine.jsonl", "engine/engine-20260928T201500.jsonl", "engine/engine.log",
		"daemon/backpackd.jsonl", "daemon/backpackd.log", "timeline.txt", "macos-power.txt", "status.json", "doctor.txt", "config.toml",
	} {
		if _, ok := files["backpack-logs-20260928T211000/"+name]; !ok {
			t.Errorf("bundle lacks %s (has %v)", name, keys(files))
		}
	}
	for name, data := range files {
		if strings.Contains(data, "topsecret-xyz") {
			t.Errorf("%s leaks the ntfy topic", name)
		}
	}
	p := "backpack-logs-20260928T211000/"
	if !strings.Contains(files[p+"config.toml"], `ntfy_topic = "<redacted>"`) {
		t.Errorf("config.toml:\n%s", files[p+"config.toml"])
	}
	if !strings.Contains(files[p+"timeline.txt"], "net.note") { // the bundle timeline includes debug
		t.Errorf("timeline.txt:\n%s", files[p+"timeline.txt"])
	}
	if !strings.Contains(files[p+"macos-power.txt"], "sleep     Entering Sleep state due to 'Clamshell Sleep'") {
		t.Errorf("macos-power.txt:\n%s", files[p+"macos-power.txt"])
	}
	var st map[string]any
	if err := json.Unmarshal([]byte(files[p+"status.json"]), &st); err != nil || st["running"] != false {
		t.Errorf("status.json is not the status JSON (%v):\n%s", err, files[p+"status.json"])
	}
	if !strings.Contains(files[p+"MANIFEST.txt"], "status --json exited 3") {
		t.Errorf("MANIFEST.txt:\n%s", files[p+"MANIFEST.txt"])
	}
	if !strings.Contains(files[p+"doctor.txt"], "doctor") {
		t.Errorf("doctor.txt:\n%s", files[p+"doctor.txt"])
	}

	// The default destination is the working directory.
	te2 := newTestEnv(t)
	te2.ex.On(pmsetKey, "")
	wd, _ := os.Getwd()
	tmp := t.TempDir()
	if err := os.Chdir(tmp); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(wd)
	if code := te2.run("logs", "--bundle"); code != exitOK {
		t.Fatalf("exit %d: %s", code, te2.errb)
	}
	if _, err := os.Stat(filepath.Join(tmp, "backpack-logs-20260928T211000.tar.gz")); err != nil {
		t.Fatal(err)
	}
}

func readTarGz(t *testing.T, path string) map[string]string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	out := map[string]string{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(tr)
		out[h.Name] = string(b)
	}
}

func keys(m map[string]string) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestParseSince(t *testing.T) {
	now := testNow // 21:10 local
	cases := map[string]time.Time{
		"":                          now.Add(-3 * time.Hour),
		"90m":                       now.Add(-90 * time.Minute),
		"2h":                        now.Add(-2 * time.Hour),
		"1d":                        now.Add(-24 * time.Hour),
		"1d12h":                     now.Add(-36 * time.Hour),
		"20:45":                     at("20:45:00"),
		"22:00":                     at("22:00:00").AddDate(0, 0, -1),
		"2026-09-27 08:00":          time.Date(2026, 9, 27, 8, 0, 0, 0, time.Local),
		"2026-09-28T12:00:00-04:00": time.Date(2026, 9, 28, 16, 0, 0, 0, time.UTC),
	}
	for in, want := range cases {
		got, err := parseSince(in, now)
		if err != nil || !got.Equal(want) {
			t.Errorf("parseSince(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, bad := range []string{"soon", "1dx", "25:00"} {
		if _, err := parseSince(bad, now); err == nil {
			t.Errorf("parseSince(%q) accepted", bad)
		}
	}
}

func TestExtractOptionalFlag(t *testing.T) {
	cases := []struct {
		in    []string
		rest  string
		set   bool
		value string
	}{
		{[]string{"--since", "2h"}, "--since 2h", false, ""},
		{[]string{"--bundle"}, "", true, ""},
		{[]string{"--bundle", "/tmp/x.tgz", "--since", "1d"}, "--since 1d", true, "/tmp/x.tgz"},
		{[]string{"--bundle", "--since", "1d"}, "--since 1d", true, ""},
		{[]string{"-bundle=out.tgz"}, "", true, "out.tgz"},
		{[]string{"--bundles"}, "--bundles", false, ""},
	}
	for _, c := range cases {
		rest, set, value := extractOptionalFlag(c.in, "bundle")
		if strings.Join(rest, " ") != c.rest || set != c.set || value != c.value {
			t.Errorf("%v: got %v %v %q", c.in, rest, set, value)
		}
	}
}
