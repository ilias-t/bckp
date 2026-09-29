package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ilias-t/bckp/internal/config"
	"github.com/ilias-t/bckp/internal/contracts"
	"github.com/ilias-t/bckp/internal/fakes"
	"github.com/ilias-t/bckp/internal/humanize"
)

func TestReadKernel(t *testing.T) {
	te := newTestEnv(t)
	te.ex.On(ioregKey, ioregOut(true, false))
	k := readKernel(context.Background(), te.ex)
	if !k.sleepDisabled() || k.LidClosed == nil || *k.LidClosed || k.Error != "" {
		t.Fatalf("k = %+v", k)
	}
	if got := k.summary(); got != "SleepDisabled=Yes, lid open" {
		t.Fatalf("summary = %q", got)
	}
	te = newTestEnv(t)
	te.ex.On(ioregKey, "nothing here")
	empty := readKernel(context.Background(), te.ex)
	if empty.SleepDisabled != nil || empty.Error == "" {
		t.Fatalf("empty = %+v", empty)
	}
}

func TestStatusRunning(t *testing.T) {
	te := newTestEnv(t)
	s := runningSnap()
	c := 71.5
	s.Thermal = contracts.ThermalStatus{FSM: contracts.FSMHot, Reading: &contracts.ThermalReading{State: contracts.ThermalSerious, CPUC: &c}, HotSince: testNow.Add(-5 * 60e9)}
	s.SleepPending = &contracts.SleepPending{Trigger: contracts.SleepTriggerSerious, Reason: "hot for 10m", RemainingS: 42, Abortable: true}
	s.Power = &contracts.PowerReading{Percent: 80, LidClosed: true}
	s.Errors = map[string]string{"network": "probe failed"}
	te.engine = fakes.NewEngineClient(s)
	te.ex.On(ioregKey, ioregOut(true, true))
	if code := te.run("status"); code != exitOK {
		t.Fatalf("code = %d", code)
	}
	out := te.out.String()
	for _, want := range []string{
		"backpack: awake until 23:40 (pid 4242)", "HOT (serious, CPU 72°C)", "SLEEP", "in 42s",
		"80% on battery, lid closed", "watchdog, online", "lease     held", "network: probe failed",
		"kernel    SleepDisabled=Yes, lid closed",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("status lacks %q:\n%s", want, out)
		}
	}
	if te.errb.Len() != 0 {
		t.Errorf("stderr = %q", te.errb)
	}
}

func TestStatusNetworkLinkAndNudges(t *testing.T) {
	te := newTestEnv(t)
	s := runningSnap()
	s.Network = contracts.NetworkStatus{Mode: contracts.NetworkPreferHotspot, Online: true, Link: contracts.LinkHotspotWiFi,
		WiFiNudges: 2, LastWiFiNudge: testNow.Add(-60e9), LastWiFiNudgeResult: "landed_hotspot"}
	te.engine = fakes.NewEngineClient(s)
	te.ex.On(ioregKey, ioregOut(true, true))
	if code := te.run("status"); code != exitOK {
		t.Fatalf("code = %d", code)
	}
	want := "hotspot preferred, online via hotspot (Wi-Fi), 2 Wi-Fi nudges (last " +
		humanize.Clock(testNow.Add(-60e9), testNow) + ": landed_hotspot)"
	if !strings.Contains(te.out.String(), want) {
		t.Fatalf("status lacks %q:\n%s", want, te.out)
	}
}

func TestStatusNotRunningWarnsOnSleepDisabled(t *testing.T) {
	te := newTestEnv(t)
	te.ex.On(ioregKey, ioregOut(true, true))
	if code := te.run("status"); code != exitNotRunning {
		t.Fatalf("code = %d", code)
	}
	if !strings.Contains(te.out.String(), "not running") || !strings.Contains(te.out.String(), "SleepDisabled=Yes") {
		t.Fatalf("stdout = %q", te.out)
	}
	if !strings.Contains(te.errb.String(), "WARNING: SleepDisabled=Yes") || !strings.Contains(te.errb.String(), "sudo pmset -a disablesleep 0") {
		t.Fatalf("stderr = %q", te.errb)
	}
}

func TestStatusNotRunningQuietWhenSleepEnabled(t *testing.T) {
	te := newTestEnv(t)
	te.ex.On(ioregKey, ioregOut(false, false))
	if code := te.run("status"); code != exitNotRunning {
		t.Fatalf("code = %d", code)
	}
	if te.errb.Len() != 0 {
		t.Fatalf("stderr = %q", te.errb)
	}
}

func TestStatusJSON(t *testing.T) {
	te := newTestEnv(t)
	te.engine = fakes.NewEngineClient(runningSnap())
	te.ex.On(ioregKey, ioregOut(true, true))
	if code := te.run("status", "--json"); code != exitOK {
		t.Fatalf("code = %d", code)
	}
	var got statusJSON
	if err := json.Unmarshal(te.out.Bytes(), &got); err != nil {
		t.Fatalf("%v: %s", err, te.out)
	}
	if !got.Running || got.Snapshot == nil || got.Snapshot.PID != 4242 || !got.Kernel.sleepDisabled() {
		t.Fatalf("got = %+v", got)
	}

	te = newTestEnv(t)
	te.ex.OnFail(ioregKey, 1, "boom")
	if code := te.run("status", "--json"); code != exitNotRunning {
		t.Fatalf("code = %d", code)
	}
	got = statusJSON{}
	if err := json.Unmarshal(te.out.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Running || got.Snapshot != nil || got.Kernel.Error == "" {
		t.Fatalf("got = %+v", got)
	}
}

func TestStop(t *testing.T) {
	cases := []struct {
		args []string
		want *bool
	}{
		{args: nil, want: nil},
		{args: []string{"--sleep"}, want: ptr(true)},
		{args: []string{"--no-sleep"}, want: ptr(false)},
	}
	for _, c := range cases {
		te := newTestEnv(t)
		fe := fakes.NewEngineClient(runningSnap())
		te.engine = fe
		if code := te.run(append([]string{"stop"}, c.args...)...); code != exitOK {
			t.Fatalf("%v: code = %d", c.args, code)
		}
		stops := fe.Stops()
		if len(stops) != 1 || (stops[0] == nil) != (c.want == nil) || c.want != nil && *stops[0] != *c.want {
			t.Errorf("%v: stops = %v", c.args, stops)
		}
	}
	te := newTestEnv(t)
	if code := te.run("stop", "--sleep", "--no-sleep"); code != exitUsage {
		t.Fatalf("conflict code = %d", code)
	}
}

func ptr(b bool) *bool { return &b }

func TestStopNotRunningRestoresJournal(t *testing.T) {
	te := newTestEnv(t)
	te.ex.On(ioregKey, ioregOut(false, false))
	called := false
	te.wiring.RestoreJournal = func(context.Context) (bool, error) { called = true; return true, nil }
	if code := te.run("stop"); code != exitNotRunning {
		t.Fatalf("code = %d", code)
	}
	if !called || !strings.Contains(te.errb.String(), "restored network settings") {
		t.Fatalf("called=%v stderr=%q", called, te.errb)
	}
}

func TestStopNotAbortable(t *testing.T) {
	te := newTestEnv(t)
	fe := fakes.NewEngineClient(runningSnap())
	fe.SetErrs(nil, contracts.ErrNotAbortable, nil)
	te.engine = fe
	if code := te.run("stop", "--no-sleep"); code != exitErr {
		t.Fatalf("code = %d", code)
	}
	if !strings.Contains(te.errb.String(), "cannot be aborted") {
		t.Fatalf("stderr = %q", te.errb)
	}
}

func TestExtend(t *testing.T) {
	te := newTestEnv(t)
	fe := fakes.NewEngineClient(runningSnap())
	te.engine = fe
	for _, arg := range []string{"30", "1h30m", "forever"} {
		if code := te.run("extend", arg); code != exitOK {
			t.Fatalf("%s: code = %d stderr %q", arg, code, te.errb)
		}
	}
	calls := fe.Extends()
	if len(calls) != 3 || calls[0].Minutes != 30 || calls[1].Minutes != 90 || !calls[2].Forever {
		t.Fatalf("calls = %+v", calls)
	}
	if !strings.Contains(te.out.String(), "extended; awake") {
		t.Fatalf("stdout = %q", te.out)
	}
	for _, bad := range [][]string{{"extend"}, {"extend", "soon"}, {"extend", "1", "2"}} {
		if code := newTestEnv(t).run(bad...); code != exitUsage {
			t.Errorf("%v: code = %d", bad, code)
		}
	}
	if code := newTestEnv(t).run("extend", "10"); code != exitNotRunning {
		t.Errorf("not running: code = %d", code)
	}
}

func TestAttach(t *testing.T) {
	te := newTestEnv(t)
	if code := te.run("attach"); code != exitNotRunning {
		t.Fatalf("not running: code = %d", code)
	}
	te = newTestEnv(t)
	te.engine = fakes.NewEngineClient(runningSnap())
	if code := te.run("attach"); code != exitUsage || !strings.Contains(te.errb.String(), "needs a terminal") {
		t.Fatalf("non-tty: code = %d stderr %q", code, te.errb)
	}
	te.isTTY = func() bool { return true }
	called := false
	te.wiring.RunTUI = func(context.Context, contracts.EngineClient, TUIOptions) error { called = true; return nil }
	if code := te.run("attach"); code != exitOK || !called {
		t.Fatalf("tty: code = %d called=%v", code, called)
	}
}

func TestNotifyTest(t *testing.T) {
	te := newTestEnv(t)
	ntfy, banner := fakes.NewNotifier(), fakes.NewNotifier()
	var gotLog *slog.Logger
	te.wiring.NewNotifiers = func(_ config.Config, log *slog.Logger) []NamedNotifier {
		gotLog = log
		return []NamedNotifier{{"ntfy", ntfy}, {"macOS", banner}, {"log", lognotifier{log}}}
	}
	te.environ = []string{"BACKPACK_NOTIFY_NTFY_TOPIC=supersecrettopic"}
	if code := te.run("notify", "test"); code != exitOK {
		t.Fatalf("code = %d stderr %q", code, te.errb)
	}
	if len(ntfy.Sent()) != 1 || ntfy.Sent()[0].Title != "backpack test" || len(banner.Sent()) != 1 {
		t.Fatalf("sent = %+v / %+v", ntfy.Sent(), banner.Sent())
	}
	out := te.out.String()
	if !strings.Contains(out, "backpack: test sent via ntfy ✓ · macOS ✓ · log ✓") {
		t.Fatalf("stdout = %q", out)
	}
	if strings.Contains(out, "supersecrettopic") || !strings.Contains(out, "su************ic") {
		t.Fatalf("topic not masked: %q", out)
	}
	// The log backend writes to cli.jsonl, never to the terminal.
	if te.errb.Len() != 0 || gotLog == nil {
		t.Fatalf("stderr = %q", te.errb)
	}
	data, err := os.ReadFile(filepath.Join(te.user.LogDir, "cli.jsonl"))
	if err != nil || !strings.Contains(string(data), `"src":"cli"`) || !strings.Contains(string(data), "backpack test") {
		t.Fatalf("cli.jsonl: %v %s", err, data)
	}
	if strings.Contains(string(data), "supersecrettopic") {
		t.Fatal("topic leaked into cli.jsonl")
	}

	ntfy.SetErr(errors.New("HTTP 500 for supersecrettopic"))
	te.out.Reset()
	if code := te.run("notify", "test"); code != exitErr {
		t.Fatalf("fail: code=%d", code)
	}
	if e := te.errb.String(); !strings.Contains(e, "ntfy ✗ (HTTP 500 for <redacted>) · macOS ✓ · log ✓") || strings.Contains(e, "supersecrettopic") {
		t.Fatalf("fail: stderr %q", e)
	}
	if code := te.run("notify", "bogus"); code != exitUsage {
		t.Fatalf("usage code = %d", code)
	}
}

// lognotifier stands in for notify/log (which the CLI cannot import).
type lognotifier struct{ log *slog.Logger }

func (l lognotifier) Send(_ context.Context, n contracts.Notification) error {
	l.log.Info("notification", "title", n.Title, "priority", n.Priority)
	return nil
}

func TestSimulatePassesScript(t *testing.T) {
	te := newTestEnv(t)
	var got string
	te.wiring.Simulate = func(_ context.Context, script string, _ config.Config, out io.Writer) error {
		got = script
		_, err := out.Write([]byte("simulated\n"))
		return err
	}
	if code := te.run("simulate", "nominal x4 serious x40 critical"); code != exitOK {
		t.Fatalf("code = %d stderr %q", code, te.errb)
	}
	if got != "nominal x4 serious x40 critical" || te.out.String() != "simulated\n" {
		t.Fatalf("script=%q out=%q", got, te.out)
	}
	te.wiring.SimulateGrammar = "GRAMMAR\n"
	if code := te.run("simulate"); code != exitUsage || !strings.HasSuffix(te.errb.String(), "GRAMMAR\n") {
		t.Fatalf("empty code = %d stderr %q", code, te.errb)
	}
	te.out.Reset()
	if code := te.run("simulate", "--help"); code != exitOK || !strings.HasSuffix(te.out.String(), "GRAMMAR\n") {
		t.Fatalf("--help code = %d out %q", code, te.out)
	}
	te.errb.Reset()
	te.wiring.Simulate = func(context.Context, string, config.Config, io.Writer) error {
		return fmt.Errorf("%w: token 1 \"foo\": unknown token", contracts.ErrBadScript)
	}
	if code := te.run("simulate", "foo"); code != exitUsage ||
		!strings.HasPrefix(te.errb.String(), "backpack: simulate: bad script: token 1 \"foo\": unknown token\n\nGRAMMAR\n") {
		t.Fatalf("bad script code = %d stderr %q", code, te.errb)
	}
}

func TestConfigInitPathShow(t *testing.T) {
	te := newTestEnv(t)
	if code := te.run("config", "path"); code != exitOK {
		t.Fatalf("path code = %d", code)
	}
	if strings.TrimSpace(te.out.String()) != te.user.ConfigFile || !strings.Contains(te.errb.String(), "not created yet") {
		t.Fatalf("path out=%q err=%q", te.out, te.errb)
	}
	te.out.Reset()
	if code := te.run("config", "init"); code != exitOK {
		t.Fatalf("init code = %d stderr %q", code, te.errb)
	}
	fi, err := os.Stat(te.user.ConfigFile)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("config file: %v %v", fi, err)
	}
	data, _ := os.ReadFile(te.user.ConfigFile)
	if _, err := config.Parse(data); err != nil {
		t.Fatalf("init wrote invalid config: %v", err)
	}
	if code := te.run("config", "init"); code != exitErr {
		t.Fatalf("second init code = %d", code)
	}
	if code := te.run("config", "init", "--force"); code != exitOK {
		t.Fatalf("force init code = %d", code)
	}
	te.out.Reset()
	te.environ = []string{"BACKPACK_SESSION_POLL=7s", "BACKPACK_NOTIFY_NTFY_TOPIC=secret-topic-42"}
	if code := te.run("config", "show"); code != exitOK {
		t.Fatalf("show code = %d", code)
	}
	if !strings.Contains(te.out.String(), `poll = "7s"`) || strings.Contains(te.out.String(), "secret-topic-42") ||
		!strings.Contains(te.out.String(), `ntfy_topic = "se***********42"`) {
		t.Fatalf("show = %s", te.out)
	}
	bad := filepath.Join(t.TempDir(), "bad.toml")
	os.WriteFile(bad, []byte("[session]\nbogus = 1\n"), 0o600)
	if code := te.run("config", "show", "--config", bad); code != exitErr {
		t.Fatalf("bad show code = %d", code)
	}
}
