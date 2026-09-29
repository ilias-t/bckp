package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ilias-t/bckp/internal/config"
	"github.com/ilias-t/bckp/internal/contracts"
	"github.com/ilias-t/bckp/internal/fakes"
	"github.com/ilias-t/bckp/internal/humanize"
	"github.com/ilias-t/bckp/internal/paths"
	"github.com/ilias-t/bckp/internal/providers/power"
)

// testNow is the fixed wall clock for tests.
var testNow = time.Date(2026, 9, 28, 21, 10, 0, 0, time.Local)

// testEnv is an env with every OS edge faked. It never touches the real
// home, /Library, sudo, launchctl or pmset.
type testEnv struct {
	*env
	out, errb *bytes.Buffer
	ex        *fakes.Exec
	engine    contracts.EngineClient
	mu        sync.Mutex
	chowns    []string
	spawned   [][]string
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	te := &testEnv{out: &bytes.Buffer{}, errb: &bytes.Buffer{}, ex: fakes.NewExec()}
	stopped := fakes.NewEngineClient(contracts.Snapshot{})
	stopped.Drop()
	te.engine = stopped
	home := t.TempDir()
	te.env = &env{
		prog:       "backpack",
		stdin:      strings.NewReader(""),
		stdout:     te.out,
		stderr:     te.errb,
		environ:    []string{},
		isTTY:      func() bool { return false },
		exec:       te.ex,
		user:       paths.ForHome(home),
		userName:   "tester",
		root:       t.TempDir(),
		getuid:     func() int { return 501 },
		executable: func() (string, error) { return "/nonexistent/backpack", nil },
		chown: func(p string, uid, gid int) error {
			te.mu.Lock()
			defer te.mu.Unlock()
			te.chowns = append(te.chowns, p)
			return nil
		},
		ownerOf:   func(os.FileInfo) (int, int) { return 0, 0 },
		newClient: func(string) contracts.EngineClient { return te.engine },
		spawn: func(args []string, logPath string) (*child, error) {
			return nil, errors.New("spawn not expected in this test")
		},
		now:   func() time.Time { return testNow },
		sleep: func(time.Duration) {},
		signalContext: func(ctx context.Context) (context.Context, context.CancelFunc) {
			return context.WithCancel(ctx)
		},
		pollInterval: time.Millisecond,
		readyTimeout: 300 * time.Millisecond,
	}
	return te
}

func (te *testEnv) run(args ...string) int { return run(te.env, args) }

// ioregKey is the fake Exec key for the kernel state query.
var ioregKey = contracts.CommandLine(paths.BinIoreg, power.ClamshellArgs...)

func ioregOut(sleepDisabled, lidClosed bool) string {
	yn := func(b bool) string {
		if b {
			return "Yes"
		}
		return "No"
	}
	return `+-o IOPMrootDomain  <class IOPMrootDomain, id 0x100000259>
  | {
  |   "AppleClamshellCausesSleep" = Yes
  |   "AppleClamshellState" = ` + yn(lidClosed) + `
  |   "SleepDisabled" = ` + yn(sleepDisabled) + `
  | }
`
}

func runningSnap() contracts.Snapshot {
	return contracts.Snapshot{
		At:  testNow,
		PID: 4242,
		Session: contracts.SessionStatus{
			StartedAt: testNow.Add(-time.Hour), EndsAt: time.Date(2026, 9, 28, 23, 40, 0, 0, time.Local),
			CapAt: testNow.Add(11 * time.Hour), RemainingS: 9000,
		},
		Thermal: contracts.ThermalStatus{FSM: contracts.FSMNominal},
		Lease:   contracts.LeaseStatus{Held: true, ID: "L1", Expires: testNow.Add(90 * time.Second)},
		Network: contracts.NetworkStatus{Mode: contracts.NetworkWatchdog, Online: true},
	}
}

func TestParseSessionDuration(t *testing.T) {
	cases := []struct {
		in      string
		want    time.Duration
		forever bool
		bad     bool
	}{
		{in: "90", want: 90 * time.Minute},
		{in: "2h", want: 2 * time.Hour},
		{in: "1h30m", want: 90 * time.Minute},
		{in: "45m", want: 45 * time.Minute},
		{in: "forever", forever: true},
		{in: "FOREVER", forever: true},
		{in: "0", bad: true},
		{in: "-5", bad: true},
		{in: "30s", bad: true},
		{in: "abc", bad: true},
		{in: "", bad: true},
		{in: "99999999", bad: true},
		{in: "1.5", bad: true},
	}
	for _, c := range cases {
		d, forever, err := parseSessionDuration(c.in)
		if c.bad {
			if err == nil {
				t.Errorf("%q: want error, got %v %v", c.in, d, forever)
			}
			continue
		}
		if err != nil || d != c.want || forever != c.forever {
			t.Errorf("%q = %v, %v, %v; want %v, %v", c.in, d, forever, err, c.want, c.forever)
		}
	}
}

// choiceString renders flag-given choices as "duration=90m hotspot=on".
func choiceString(c choices) string {
	var parts []string
	for _, o := range sessionOptions {
		if ch, ok := c[o.name]; ok {
			parts = append(parts, o.name+"="+ch.value)
		}
	}
	return strings.Join(parts, " ")
}

func TestParseStartArgs(t *testing.T) {
	type want struct {
		durArg                         string
		detach, foreground, noTUI, yes bool
		configPath                     string
		choices                        string
	}
	cases := []struct {
		args []string
		want want
		code int
		bad  bool
	}{
		{args: nil, want: want{}},
		{args: []string{"90"}, want: want{durArg: "90", choices: "duration=1h30m"}},
		{args: []string{"90", "-d"}, want: want{durArg: "90", detach: true, choices: "duration=1h30m"}},
		{args: []string{"--detach", "2h", "--hotspot=on"}, want: want{durArg: "2h", detach: true, choices: "duration=2h hotspot=on"}},
		{args: []string{"-f", "--no-tui", "forever", "--config", "/x.toml"}, want: want{durArg: "forever", foreground: true, noTUI: true, configPath: "/x.toml", choices: "duration=forever"}},
		{args: []string{"--hotspot=auto", "--no-watchdog", "-y"}, want: want{yes: true, choices: "hotspot=auto watchdog=off"}},
		{args: []string{"--hotspot", "off", "--hotspot=off", "--yes", "45m"}, want: want{durArg: "45m", yes: true, choices: "duration=45m hotspot=off"}},
		{args: []string{"--hotspot=off", "--no-watchdog"}, want: want{choices: "hotspot=off watchdog=off"}},
		{args: []string{"90", "2h"}, bad: true, code: exitUsage},
		{args: []string{"--bogus"}, bad: true, code: exitUsage},
		{args: []string{"--no-network"}, bad: true, code: exitUsage},
		{args: []string{"--prefer-hotspot"}, bad: true, code: exitUsage},
		{args: []string{"--no-watchdog", "--watchdog"}, bad: true, code: exitUsage},
		{args: []string{"--hotspot=on", "--hotspot=off"}, bad: true, code: exitUsage},
		{args: []string{"--hotspot=maybe"}, bad: true, code: exitUsage},
		{args: []string{"--watchdog=false"}, bad: true, code: exitUsage},
		{args: []string{"--sources", "duration=flag"}, bad: true, code: exitUsage}, // _engine only
		{args: []string{"-h"}, bad: true, code: exitOK},
	}
	for _, c := range cases {
		te := newTestEnv(t)
		got, code, ok := parseStart(te.env, "", c.args)
		if c.bad {
			if ok || code != c.code {
				t.Errorf("%v: ok=%v code=%d, want code %d", c.args, ok, code, c.code)
			}
			continue
		}
		g := want{got.durArg, got.detach, got.foreground, got.noTUI, got.yes, got.configPath, choiceString(got.choices)}
		if !ok || g != c.want {
			t.Errorf("%v = %+v (ok=%v, stderr %q); want %+v", c.args, g, ok, te.errb, c.want)
		}
	}
	te := newTestEnv(t)
	if _, _, ok := parseStart(te.env, "", []string{"--hotspot=on", "--hotspot=off"}); ok ||
		!strings.Contains(te.errb.String(), "--hotspot=on and --hotspot=off conflict") {
		t.Errorf("conflict message: %q", te.errb)
	}
}

func TestParseInterspersedDoubleDash(t *testing.T) {
	te := newTestEnv(t)
	fs := newFlags(te.env, "x")
	d := fs.Bool("d", false, "")
	pos, err := parseInterspersed(fs.FlagSet, []string{"a", "-d", "--", "-b"})
	if err != nil || !*d || strings.Join(pos, ",") != "a,-b" {
		t.Fatalf("pos=%v d=%v err=%v", pos, *d, err)
	}
}

func TestDecideStartMode(t *testing.T) {
	cases := []struct {
		name string
		o    startOpts
		tty  bool
		want startMode
		bad  bool
	}{
		{name: "non-tty bare auto-detaches", tty: false, want: modeDetach},
		{name: "non-tty --no-tui still auto-detaches", o: startOpts{noTUI: true}, want: modeDetach},
		{name: "non-tty --detach", o: startOpts{detach: true}, want: modeDetach},
		{name: "non-tty --foreground stays", o: startOpts{foreground: true}, want: modeForeground},
		{name: "tty bare attaches TUI", tty: true, want: modeDetachTUI},
		{name: "tty --no-tui detaches", o: startOpts{noTUI: true}, tty: true, want: modeDetach},
		{name: "tty --detach", o: startOpts{detach: true}, tty: true, want: modeDetach},
		{name: "tty --foreground TUI", o: startOpts{foreground: true}, tty: true, want: modeForegroundTUI},
		{name: "tty -f --no-tui", o: startOpts{foreground: true, noTUI: true}, tty: true, want: modeForeground},
		{name: "conflict", o: startOpts{foreground: true, detach: true}, bad: true},
	}
	for _, c := range cases {
		mode, err := decideStartMode(c.o, c.tty)
		if c.bad {
			if err == nil {
				t.Errorf("%s: want error", c.name)
			}
			continue
		}
		if err != nil || mode != c.want {
			t.Errorf("%s: mode=%v err=%v; want %v", c.name, mode, err, c.want)
		}
	}
}

func TestDispatchUnknownWord(t *testing.T) {
	te := newTestEnv(t)
	if code := te.run("stauts"); code != exitUsage {
		t.Fatalf("code = %d", code)
	}
	if !strings.Contains(te.errb.String(), `unknown command or duration "stauts"`) {
		t.Fatalf("stderr = %q", te.errb)
	}
}

func TestHelpUsesProgName(t *testing.T) {
	te := newTestEnv(t)
	te.prog = progName("/usr/local/bin/bckp")
	if code := te.run("help"); code != exitOK {
		t.Fatalf("code = %d", code)
	}
	out := te.out.String()
	if !strings.Contains(out, "  bckp [DUR] [flags]") || !strings.Contains(out, "  status [--json]  ") ||
		!strings.Contains(out, "ssh host bckp 2h") || strings.Contains(out, "backpack") || strings.Contains(out, "_engine") {
		t.Fatalf("help = %s", out)
	}
	if progName("backpack") != "backpack" || progName("") != "backpack" {
		t.Error("progName")
	}
}

func TestReadyLine(t *testing.T) {
	cfg := config.Default()
	s := runningSnap()
	if got := readyLine(s, cfg, testNow); got != "awake until 23:40 (pid 4242)" {
		t.Errorf("timed, no options = %q", got)
	}
	s.Options = []contracts.SessionOption{
		{Name: "duration", Value: "2h30m", Source: "flag"},
		{Name: "hotspot", Value: "auto", Source: "default"},
		{Name: "watchdog", Value: "on", Source: "prompt"},
	}
	if got := readyLine(s, cfg, testNow); got != "awake until 23:40 · hotspot auto · watchdog on (pid 4242)" {
		t.Errorf("timed = %q", got)
	}
	s.Session.Forever, s.Session.EndsAt = true, time.Time{}
	s.Session.StartedAt, s.Session.CapAt = testNow, testNow.Add(12*time.Hour)
	want := "awake forever (12h cap at " + humanize.Clock(testNow.Add(12*time.Hour), testNow) + ") · hotspot auto · watchdog on (pid 4242)"
	if got := readyLine(s, cfg, testNow); got != want {
		t.Errorf("forever = %q, want %q", got, want)
	}
	// A request beyond the cap shows the cap.
	s = runningSnap()
	s.Session.StartedAt = testNow
	s.Session.EndsAt = testNow.Add(20 * time.Hour)
	s.Session.CapAt = testNow.Add(12 * time.Hour)
	if got := readyLine(s, cfg, testNow); !strings.Contains(got, "(12h cap) (pid 4242)") {
		t.Errorf("capped = %q", got)
	}
}

func TestEngineOptionsFromChoices(t *testing.T) {
	te := newTestEnv(t)
	o, _, ok := parseStart(te.env, "", []string{"--hotspot=off", "--no-watchdog", "60"})
	if !ok {
		t.Fatal(te.errb)
	}
	cfg := config.Default()
	o.choices.fillDefaults(cfg)
	opts, err := o.choices.engineOptions(&cfg)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Network.Watchdog || cfg.Network.PreferHotspot != config.HotspotOff || opts.Duration != time.Hour {
		t.Fatalf("cfg=%+v opts=%+v", cfg.Network, opts)
	}
	want := []contracts.SessionOption{
		{Name: "duration", Value: "1h", Source: "flag"},
		{Name: "hotspot", Value: "off", Source: "flag"},
		{Name: "watchdog", Value: "off", Source: "flag"},
	}
	if !slices.Equal(opts.Choices, want) {
		t.Fatalf("choices = %+v", opts.Choices)
	}

	o, _, _ = parseStart(te.env, "", []string{"--hotspot=on"})
	cfg = config.Default()
	o.choices.fillDefaults(cfg)
	opts, _ = o.choices.engineOptions(&cfg)
	if cfg.Network.PreferHotspot != config.HotspotOn || opts.Duration != 0 ||
		!cfg.Network.Watchdog || opts.Choices[0].Source != "default" {
		t.Fatalf("cfg=%+v opts=%+v", cfg.Network, opts)
	}
}

func TestNotWiredCommands(t *testing.T) {
	for _, args := range [][]string{{"90"}, {"simulate", "nominal x4"}, {"notify", "test"}, {"_engine", "90"}} {
		te := newTestEnv(t)
		if code := te.run(args...); code != exitErr {
			t.Errorf("%v: code = %d", args, code)
		}
		if !strings.Contains(te.errb.String(), "not wired") {
			t.Errorf("%v: stderr = %q", args, te.errb)
		}
	}
}

func TestSubcommandHelpAndBadFlag(t *testing.T) {
	te := newTestEnv(t)
	te.prog = "bckp"
	if code := te.run("logs", "--help"); code != exitOK {
		t.Fatalf("code = %d", code)
	}
	out := te.out.String()
	for _, want := range []string{"usage: bckp logs [--since 2h] [-f]\n", "  -f, --follow ", "      --since WHEN ", "      --bundle [PATH] "} {
		if !strings.Contains(out, want) {
			t.Errorf("logs --help lacks %q:\n%s", want, out)
		}
	}
	if code := te.run("version", "-h"); code != exitOK {
		t.Fatalf("version -h code = %d", code)
	}
	te.errb.Reset()
	if code := te.run("status", "--bogus"); code != exitUsage {
		t.Fatalf("code = %d", code)
	}
	if got := te.errb.String(); !strings.HasPrefix(got, "bckp: flag provided but not defined: -bogus\nusage: bckp status [--json]") {
		t.Fatalf("stderr = %q", got)
	}
}
