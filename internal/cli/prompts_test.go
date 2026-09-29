package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ilias-t/bckp/internal/config"
	"github.com/ilias-t/bckp/internal/contracts"
	"github.com/ilias-t/bckp/internal/fakes"
)

// startEnv is a test env ready to start a detached session: the engine
// answers "ready" after the single-instance check, spawn records argv, and
// a config with an ntfy topic exists (so no first-run or topic prompt),
// unless the test removes it. input is what the "terminal" types.
func startEnv(t *testing.T, tty bool, input string) *testEnv {
	t.Helper()
	te := newTestEnv(t)
	withEngine(te)
	te.isTTY = func() bool { return tty }
	te.stdin = strings.NewReader(input)
	te.engine = readyAfter(1)
	te.spawn = func(args []string, logPath string) (*child, error) {
		te.spawned = append(te.spawned, args)
		return &child{pid: 4242, exited: make(chan error), logPath: logPath}, nil
	}
	writeFile(t, te.user.ConfigFile, "# mine\n[notify]\nntfy_topic = \"t0pic\"\n", 0o600)
	return te
}

func (te *testEnv) spawnedArgs() string {
	if len(te.spawned) != 1 {
		return ""
	}
	return strings.Join(te.spawned[0], " ")
}

// TestPromptsTakeDefaultsOnEnter is the full TTY transcript for a bare
// `bckp` with Enter at every question.
func TestPromptsTakeDefaultsOnEnter(t *testing.T) {
	te := startEnv(t, true, "\n\n\n")
	if code := te.run("--no-tui"); code != exitOK {
		t.Fatalf("code = %d\nstdout %s\nstderr %s", code, te.out, te.errb)
	}
	want := "How long? [forever] (12h cap) / 90 / 2h: " +
		"Prefer iPhone hotspot over Wi-Fi? [A]uto (when plugged in) / y / n: " +
		"Watch the network (outage alerts, hotspot link recovery)? [Y/n]: " +
		"backpack: forever (12h cap) · hotspot auto · watchdog on\n" +
		"backpack: awake until 23:40 (pid 4242)\n"
	if got := te.out.String(); got != want {
		t.Fatalf("transcript:\n%s\nwant:\n%s", got, want)
	}
	if got := te.spawnedArgs(); got != "_engine --sources duration=prompt,hotspot=prompt,watchdog=prompt --hotspot=auto --watchdog forever" {
		t.Fatalf("spawned = %q", got)
	}
	t.Logf("transcript:\n%s", te.out)
}

func TestPromptsTakeAnswers(t *testing.T) {
	te := startEnv(t, true, "90\ny\nn\n")
	if code := te.run("--no-tui"); code != exitOK {
		t.Fatalf("code = %d\n%s%s", code, te.out, te.errb)
	}
	if !strings.Contains(te.out.String(), "backpack: 1h30m · hotspot on · watchdog off\n") {
		t.Fatalf("summary missing:\n%s", te.out)
	}
	if got := te.spawnedArgs(); got != "_engine --sources duration=prompt,hotspot=prompt,watchdog=prompt --hotspot=on --no-watchdog 1h30m" {
		t.Fatalf("spawned = %q", got)
	}
}

// Options given by flag (or DUR) are not asked; the rest are, in order.
func TestPromptsSkipFlaggedOptions(t *testing.T) {
	te := startEnv(t, true, "\n")
	if code := te.run("2h", "--hotspot=off", "--no-tui"); code != exitOK {
		t.Fatalf("code = %d\n%s%s", code, te.out, te.errb)
	}
	out := te.out.String()
	if strings.Contains(out, "How long?") || strings.Contains(out, "Prefer iPhone") || !strings.HasPrefix(out, "Watch the network (outage alerts, hotspot link recovery)? [Y/n]: ") {
		t.Fatalf("transcript:\n%s", out)
	}
	if got := te.spawnedArgs(); got != "_engine --sources duration=flag,hotspot=flag,watchdog=prompt --hotspot=off --watchdog 2h" {
		t.Fatalf("spawned = %q", got)
	}
}

// Without the hotspot's network service (no iPhone ever connected over
// USB) the hotspot question is skipped and its default kept.
func TestPromptsSkipHotspotWithoutService(t *testing.T) {
	for _, c := range []struct {
		list string
		ask  bool
	}{
		{"An asterisk (*) denotes that a network service is disabled.\nWi-Fi\nThunderbolt Bridge\n", false},
		{"An asterisk (*) denotes that a network service is disabled.\nWi-Fi\n*iPhone USB\n", true},
	} {
		te := startEnv(t, true, "\n\n\n")
		te.ex.On("/usr/sbin/networksetup -listallnetworkservices", c.list)
		if code := te.run("--no-tui"); code != exitOK {
			t.Fatalf("code = %d\n%s%s", code, te.out, te.errb)
		}
		out := te.out.String()
		if strings.Contains(out, "Prefer iPhone hotspot") != c.ask || !strings.Contains(out, "Watch the network") {
			t.Fatalf("%q: transcript:\n%s", c.list, out)
		}
		if !c.ask && !strings.Contains(te.spawnedArgs(), "hotspot=default") {
			t.Fatalf("spawned = %q", te.spawnedArgs())
		}
	}
}

// The prompt shows the config default in brackets, and Enter takes it.
func TestPromptsShowConfigDefaults(t *testing.T) {
	te := startEnv(t, true, "\n\n\n")
	writeFile(t, te.user.ConfigFile, "[notify]\nntfy_topic = \"t0pic\"\n[network]\nprefer_hotspot = \"on\"\nwatchdog = false\n[session]\nmax_duration = \"8h\"\n", 0o600)
	if code := te.run("--no-tui"); code != exitOK {
		t.Fatalf("code = %d\n%s%s", code, te.out, te.errb)
	}
	out := te.out.String()
	for _, want := range []string{
		"How long? [forever] (8h cap) / 90 / 2h: ",
		"Prefer iPhone hotspot over Wi-Fi? a (auto, when plugged in) / [Y]es / n: ",
		"Watch the network (outage alerts, hotspot link recovery)? [y/N]: ",
		"backpack: forever (8h cap) · hotspot on · watchdog off\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("lacks %q:\n%s", want, out)
		}
	}
}

func TestPromptReasksInvalidAnswer(t *testing.T) {
	te := startEnv(t, true, "soon\n45\nmaybe\na\n\n")
	if code := te.run("--no-tui"); code != exitOK {
		t.Fatalf("code = %d\n%s%s", code, te.out, te.errb)
	}
	out := te.out.String()
	if strings.Count(out, "How long?") != 2 || strings.Count(out, "Prefer iPhone") != 2 || !strings.Contains(out, "  hotspot: want auto, on or off") {
		t.Fatalf("transcript:\n%s", out)
	}
	if !strings.Contains(out, "backpack: 45m · hotspot auto · watchdog on\n") {
		t.Fatalf("summary:\n%s", out)
	}
	t.Logf("transcript:\n%s", te.out)
}

func TestPromptEOFStartsNothing(t *testing.T) {
	te := startEnv(t, true, "90\n")
	if code := te.run("--no-tui"); code != exitErr {
		t.Fatalf("code = %d", code)
	}
	if len(te.spawned) != 0 || !strings.Contains(te.errb.String(), "nothing started") {
		t.Fatalf("spawned %v stderr %q", te.spawned, te.errb)
	}
}

// blockingStdin is a reader that never returns: reading it would hang.
func blockingStdin(t *testing.T) io.Reader {
	r, w := io.Pipe()
	t.Cleanup(func() { w.Close() })
	return r
}

// runWithin runs args and fails if the run blocks (e.g. on stdin).
func runWithin(t *testing.T, te *testEnv, args ...string) int {
	t.Helper()
	done := make(chan int, 1)
	go func() { done <- te.run(args...) }()
	select {
	case code := <-done:
		return code
	case <-time.After(5 * time.Second):
		t.Fatal("run blocked (on stdin?)")
		return -1
	}
}

// A caller without a terminal (an agent's shell, ssh from a phone) is
// never asked anything and never blocks, and the one line lists every
// effective option.
func TestNonTTYNeverReadsStdin(t *testing.T) {
	te := startEnv(t, false, "")
	te.stdin = blockingStdin(t)
	snap := runningSnap()
	snap.Options = []contracts.SessionOption{
		{Name: "duration", Value: "forever", Source: "default"},
		{Name: "hotspot", Value: "auto", Source: "default"},
		{Name: "watchdog", Value: "on", Source: "default"},
	}
	te.engine = &scriptClient{status: func(i int) (contracts.Snapshot, error) {
		if i == 1 {
			return notRunning()
		}
		return snap, nil
	}}
	if code := runWithin(t, te); code != exitOK {
		t.Fatalf("code = %d stderr %q", code, te.errb)
	}
	if got := te.out.String(); got != "backpack: awake until 23:40 · hotspot auto · watchdog on (pid 4242)\n" {
		t.Fatalf("stdout = %q", got)
	}
	if got := te.spawnedArgs(); got != "_engine --sources duration=default,hotspot=default,watchdog=default --hotspot=auto --watchdog forever" {
		t.Fatalf("spawned = %q", got)
	}
}

// --yes in a terminal: no questions (stdin is never read), but the
// summary still shows which defaults were taken.
func TestYesSkipsPrompts(t *testing.T) {
	te := startEnv(t, true, "")
	te.stdin = blockingStdin(t)
	if code := runWithin(t, te, "-y", "--no-tui"); code != exitOK {
		t.Fatalf("code = %d stderr %q", code, te.errb)
	}
	out := te.out.String()
	if strings.Contains(out, "?") || !strings.HasPrefix(out, "backpack: forever (12h cap) · hotspot auto · watchdog on\n") {
		t.Fatalf("stdout = %q", out)
	}
}

func TestNonTTYWithoutConfigNeverSetsUp(t *testing.T) {
	te := startEnv(t, false, "")
	te.stdin = blockingStdin(t)
	os.Remove(te.user.ConfigFile)
	if code := runWithin(t, te); code != exitOK {
		t.Fatalf("code = %d stderr %q", code, te.errb)
	}
	if _, err := os.Stat(te.user.ConfigFile); err == nil {
		t.Fatal("no config may be written without a terminal")
	}
	if te.errb.Len() != 0 {
		t.Fatalf("stderr %q", te.errb)
	}
}

// ---- daemon ----

func missingDaemon(te *testEnv) *fakes.SleepGuard {
	g := fakes.NewSleepGuard(nil)
	g.SetStatus(nil, contracts.ErrDaemonUnavailable)
	te.wiring.NewGuard = func() contracts.SleepGuard { return g }
	return g
}

func TestNoDaemonNonTTYRefuses(t *testing.T) {
	te := startEnv(t, false, "")
	te.stdin = blockingStdin(t)
	missingDaemon(te)
	te.interactive = func(context.Context, string, ...string) error {
		t.Fatal("must not run sudo without a terminal")
		return nil
	}
	if code := runWithin(t, te); code != exitErr {
		t.Fatalf("code = %d", code)
	}
	if len(te.spawned) != 0 || !strings.Contains(te.errb.String(), "sudo /nonexistent/backpack install") {
		t.Fatalf("spawned %v stderr %q", te.spawned, te.errb)
	}
}

func TestNoDaemonTTYOffersInstall(t *testing.T) {
	te := startEnv(t, true, "\n\n\n\n")
	g := missingDaemon(te)
	var ran []string
	te.interactive = func(_ context.Context, name string, args ...string) error {
		ran = append([]string{name}, args...)
		g.SetStatus(&contracts.GuardStatus{Backstop: contracts.BackstopIdle}, nil)
		return nil
	}
	if code := te.run("--no-tui"); code != exitOK {
		t.Fatalf("code = %d\n%s%s", code, te.out, te.errb)
	}
	if strings.Join(ran, " ") != "/usr/bin/sudo /nonexistent/backpack install --no-hint" {
		t.Fatalf("ran %q", ran)
	}
	out := te.out.String()
	want := "backpackd, the small root helper that keeps a closed lid awake, is not installed or not answering.\n" +
		"Install it now with `sudo /nonexistent/backpack install`? [Y/n]: backpackd is answering ✓\n" +
		"How long? [forever]"
	if !strings.HasPrefix(out, want) {
		t.Fatalf("transcript:\n%s", out)
	}
	if len(te.spawned) != 1 {
		t.Fatal("session not started after the install")
	}
	t.Logf("transcript:\n%s", te.out)
}

func TestNoDaemonTTYDeclined(t *testing.T) {
	te := startEnv(t, true, "n\n")
	missingDaemon(te)
	te.interactive = func(context.Context, string, ...string) error { t.Fatal("declined, yet sudo ran"); return nil }
	if code := te.run(); code != exitErr {
		t.Fatalf("code = %d", code)
	}
	if len(te.spawned) != 0 || !strings.Contains(te.errb.String(), "not starting") {
		t.Fatalf("stderr %q", te.errb)
	}
	if strings.Contains(te.out.String(), "How long?") {
		t.Fatal("options must not be asked when the session cannot start")
	}
}

func TestNoDaemonInstallFails(t *testing.T) {
	te := startEnv(t, true, "y\n")
	missingDaemon(te)
	te.interactive = func(context.Context, string, ...string) error { return errors.New("exit status 1") }
	if code := te.run(); code != exitErr || len(te.spawned) != 0 {
		t.Fatalf("code = %d spawned %v", code, te.spawned)
	}
	if !strings.Contains(te.errb.String(), "install failed: exit status 1; not starting") {
		t.Fatalf("stderr %q", te.errb)
	}
}

func TestNoDaemonStillDownAfterInstall(t *testing.T) {
	te := startEnv(t, true, "\n")
	missingDaemon(te)
	te.interactive = func(context.Context, string, ...string) error { return nil }
	if code := te.run(); code != exitErr || len(te.spawned) != 0 {
		t.Fatalf("code = %d spawned %v", code, te.spawned)
	}
	if !strings.Contains(te.errb.String(), "still does not answer") {
		t.Fatalf("stderr %q", te.errb)
	}
}

// ---- first run and the ntfy topic ----

func TestFirstRunGeneratesTopic(t *testing.T) {
	te := startEnv(t, true, "g\n\n\n\n")
	os.Remove(te.user.ConfigFile)
	te.random = strings.NewReader(strings.Repeat("\x00\x01\x02\x03\x04\x05", 4))
	if code := te.run("--no-tui"); code != exitOK {
		t.Fatalf("code = %d\n%s%s", code, te.out, te.errb)
	}
	out := te.out.String()
	want := "No backpack config yet. Quick setup (Enter skips it):\n" +
		"With the lid closed you won't see macOS banners; heat alerts reach you on your phone\n" +
		"through the ntfy app (no account needed).\n" +
		"ntfy topic for phone alerts: paste one, [g]enerate a random one, or Enter to skip: " +
		"ntfy topic: abcdefabcdefabcdefabcdef\n" +
		"Subscribe to it in the ntfy app (https://ntfy.sh/abcdefabcdefabcdefabcdef), then check with `backpack notify test`.\n" +
		"wrote " + te.user.ConfigFile + "\n" +
		"How long? [forever]"
	if !strings.HasPrefix(out, want) {
		t.Fatalf("transcript:\n%s\nwant prefix:\n%s", out, want)
	}
	cfg, err := config.Load(config.LoadOptions{Path: te.user.ConfigFile, MustExist: true})
	if err != nil || cfg.Notify.NtfyTopic != "abcdefabcdefabcdefabcdef" {
		t.Fatalf("config: %v topic %q", err, cfg.Notify.NtfyTopic)
	}
	if fi, _ := os.Stat(te.user.ConfigFile); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode())
	}
	// Only what the user chose is set; every other key keeps coming from
	// the defaults.
	if data, _ := os.ReadFile(te.user.ConfigFile); !strings.Contains(string(data), "[notify]\nntfy_topic = \"abcdefabcdefabcdefabcdef\"\n") ||
		strings.Contains(string(data), "serious_sleep_after") || strings.Contains(string(data), "\nmax_duration") {
		t.Fatalf("config:\n%s", data)
	}
	if strings.Contains(te.errb.String(), "ntfy_topic is not set") {
		t.Fatal("the topic was just set; no warning")
	}
	t.Logf("transcript:\n%s", te.out)
}

func TestFirstRunSkipTopicAsksOnce(t *testing.T) {
	te := startEnv(t, true, "\n\n\n\n")
	os.Remove(te.user.ConfigFile)
	if code := te.run("--no-tui"); code != exitOK {
		t.Fatalf("code = %d\n%s%s", code, te.out, te.errb)
	}
	if n := strings.Count(te.out.String(), "ntfy topic for phone alerts"); n != 1 {
		t.Fatalf("topic asked %d times:\n%s", n, te.out)
	}
	cfg, err := config.Load(config.LoadOptions{Path: te.user.ConfigFile, MustExist: true})
	if err != nil || cfg.Notify.NtfyTopic != "" {
		t.Fatalf("config: %v topic %q", err, cfg.Notify.NtfyTopic)
	}
}

// With macOS notifications on (the default), an empty topic is not
// mentioned at start (doctor keeps the tip): no nagging at every start.
func TestEmptyTopicWithMacOSNotificationsIsQuiet(t *testing.T) {
	te := startEnv(t, true, "\n\n\n")
	writeFile(t, te.user.ConfigFile, "[session]\npoll = \"20s\"\n", 0o600)
	if code := te.run("--no-tui"); code != exitOK {
		t.Fatalf("code = %d\n%s%s", code, te.out, te.errb)
	}
	if te.errb.Len() != 0 {
		t.Fatalf("stderr %q", te.errb)
	}
	if strings.Contains(te.out.String(), "ntfy topic for phone alerts") {
		t.Fatalf("topic asked:\n%s", te.out)
	}
	if len(te.spawned) != 1 {
		t.Fatal("session not started")
	}
}

// With macOS notifications off, nothing would reach the user: warn and
// offer to set a topic.
func TestEmptyTopicOfferSavesIntoConfig(t *testing.T) {
	te := startEnv(t, true, "bad topic!\npasted-topic\n\n\n\n")
	writeFile(t, te.user.ConfigFile, "# keep me\n[notify]\nmacos = false\n", 0o600)
	if code := te.run("--no-tui"); code != exitOK {
		t.Fatalf("code = %d\n%s%s", code, te.out, te.errb)
	}
	if !strings.Contains(te.errb.String(), "notify.ntfy_topic is not set") {
		t.Fatalf("stderr %q", te.errb)
	}
	if !strings.Contains(te.out.String(), "  a topic is 1-64 letters, digits, - or _\n") {
		t.Fatalf("invalid topic not re-asked:\n%s", te.out)
	}
	data, _ := os.ReadFile(te.user.ConfigFile)
	if string(data) != "# keep me\n[notify]\nntfy_topic = \"pasted-topic\"\nmacos = false\n" {
		t.Fatalf("config:\n%s", data)
	}
}

// ---- detach log ----

func TestDetachLogsSessionDetach(t *testing.T) {
	te := startEnv(t, false, "")
	te.environ = []string{"SSH_CONNECTION=1.2.3.4 5 6.7.8.9 22"}
	if code := te.run("90", "--hotspot=off"); code != exitOK {
		t.Fatalf("code = %d stderr %q", code, te.errb)
	}
	data, err := os.ReadFile(filepath.Join(te.user.LogDir, "cli.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	line := string(data)
	for _, want := range []string{
		`"src":"cli"`, `"event":"session.detach"`, `"mode":"auto-detach"`, `"tty":false`, `"ssh":true`,
		`"pid":4242`, `"result":"ok"`, `"hotspot":"off (flag)"`, `"duration":"1h30m (flag)"`, `"watchdog":"on (default)"`,
	} {
		if !strings.Contains(line, want) {
			t.Errorf("lacks %s:\n%s", want, line)
		}
	}
	if strings.Contains(line, "t0pic") {
		t.Fatal("topic leaked")
	}
	if _, err := os.Stat(filepath.Join(te.user.LogDir, "engine.jsonl")); err == nil {
		t.Fatal("the CLI must not write the engine's log file")
	}
}

// ---- status, abort, attach ----

func TestStatusShowsOptionsAndSources(t *testing.T) {
	te := newTestEnv(t)
	snap := runningSnap()
	snap.Options = []contracts.SessionOption{
		{Name: "duration", Value: "2h30m", Source: "flag"},
		{Name: "hotspot", Value: "auto", Source: "prompt"},
		{Name: "watchdog", Value: "on", Source: "default"},
	}
	snap.Network.Mode = contracts.NetworkHotspotAuto
	te.engine = fakes.NewEngineClient(snap)
	te.ex.On(ioregKey, ioregOut(true, true))
	if code := te.run("status"); code != exitOK {
		t.Fatalf("code %d", code)
	}
	out := te.out.String()
	for _, want := range []string{
		"backpack: awake until 23:40 · hotspot auto · watchdog on (pid 4242)",
		"  options   duration 2h30m (flag) · hotspot auto (prompt) · watchdog on (default)",
		"  network   hotspot auto (waiting for the phone), online",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("lacks %q:\n%s", want, out)
		}
	}
}

func TestAbortCommand(t *testing.T) {
	te := newTestEnv(t)
	snap := runningSnap()
	snap.SleepPending = &contracts.SleepPending{Trigger: contracts.SleepTriggerSerious, Abortable: true, RemainingS: 40}
	f := fakes.NewEngineClient(snap)
	te.engine = f
	if code := te.run("abort"); code != exitOK || !strings.Contains(te.out.String(), "pending sleep cancelled") {
		t.Fatalf("code %d out %q", code, te.out)
	}
	if f.Aborts() != 1 || len(f.Extends()) != 0 {
		t.Fatalf("aborts %d extends %v", f.Aborts(), f.Extends())
	}
	te.out.Reset()
	if code := te.run("abort"); code != exitOK || !strings.Contains(te.out.String(), "no sleep pending") {
		t.Fatalf("again: code %d out %q", code, te.out)
	}
	snap.SleepPending = &contracts.SleepPending{Trigger: contracts.SleepTriggerCritical}
	te.engine = fakes.NewEngineClient(snap)
	if code := te.run("abort"); code != exitErr || !strings.Contains(te.errb.String(), "cannot be aborted") {
		t.Fatalf("critical: code %d stderr %q", code, te.errb)
	}
	stopped := fakes.NewEngineClient(contracts.Snapshot{})
	stopped.Drop()
	te.engine = stopped
	if code := te.run("abort"); code != exitNotRunning {
		t.Fatalf("not running: code %d", code)
	}
	if code := te.run("abort", "now"); code != exitUsage {
		t.Fatalf("args: code %d", code)
	}
}

// An external SIGINT during attach detaches: exit 0, "still running".
func TestAttachInterruptedDetaches(t *testing.T) {
	te := newTestEnv(t)
	te.isTTY = func() bool { return true }
	te.engine = fakes.NewEngineClient(runningSnap())
	te.signalContext = func(ctx context.Context) (context.Context, context.CancelFunc) {
		ctx, cancel := context.WithCancel(ctx)
		cancel() // the signal already arrived
		return ctx, cancel
	}
	te.wiring.RunTUI = func(ctx context.Context, _ contracts.EngineClient, _ TUIOptions) error { return ctx.Err() }
	if code := te.run("attach"); code != exitOK {
		t.Fatalf("code %d stderr %q", code, te.errb)
	}
	if !strings.Contains(te.out.String(), "still running — `backpack attach` to return") || te.errb.Len() != 0 {
		t.Fatalf("out %q err %q", te.out, te.errb)
	}
}

// An external SIGINT in a foreground TUI session stops the engine and
// exits 0 (not "tui: context canceled").
func TestForegroundTUIInterruptedStopsCleanly(t *testing.T) {
	te := newTestEnv(t)
	te.isTTY = func() bool { return true }
	sigCtx, signal := context.WithCancel(context.Background())
	te.signalContext = func(ctx context.Context) (context.Context, context.CancelFunc) {
		ctx, cancel := context.WithCancel(ctx)
		go func() { <-sigCtx.Done(); cancel() }()
		return ctx, cancel
	}
	up := make(chan struct{})
	te.wiring.RunEngine = func(ctx context.Context, _ config.Config, _ EngineOptions) error {
		close(up)
		<-ctx.Done()
		return nil
	}
	te.engine = &scriptClient{status: func(int) (contracts.Snapshot, error) {
		select {
		case <-up:
			return runningSnap(), nil
		default:
			return notRunning()
		}
	}}
	te.wiring.RunTUI = func(ctx context.Context, _ contracts.EngineClient, o TUIOptions) error {
		if !o.Foreground {
			t.Error("foreground TUI not told it is foreground")
		}
		signal()
		<-ctx.Done()
		return ctx.Err()
	}
	if code := te.run("-f", "-y"); code != exitOK {
		t.Fatalf("code %d stderr %q", code, te.errb)
	}
	if strings.Contains(te.errb.String(), "context canceled") {
		t.Fatalf("stderr %q", te.errb)
	}
}

func TestGenerateTopic(t *testing.T) {
	te := newTestEnv(t)
	te.random = strings.NewReader("0123456789abcdefghijklmn")
	topic, err := te.generateTopic()
	if err != nil || !config.ValidTopic(topic) || len(topic) != topicLen {
		t.Fatalf("topic %q err %v", topic, err)
	}
	// Nothing identifying: the user name must not leak into a public topic.
	if strings.Contains(topic, "tester") || strings.Contains(topic, "bckp") {
		t.Fatalf("topic %q contains an identifying prefix", topic)
	}
}
