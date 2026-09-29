package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ilias-t/bckp/internal/config"
	"github.com/ilias-t/bckp/internal/contracts"
	"github.com/ilias-t/bckp/internal/fakes"
	"golang.org/x/sys/unix"
)

// scriptClient answers Status from a function of the call number.
type scriptClient struct {
	mu     sync.Mutex
	calls  int
	status func(n int) (contracts.Snapshot, error)
}

func (c *scriptClient) Status(ctx context.Context) (contracts.Snapshot, error) {
	c.mu.Lock()
	c.calls++
	n := c.calls
	c.mu.Unlock()
	return c.status(n)
}
func (c *scriptClient) Stop(context.Context, *bool) error { return nil }
func (c *scriptClient) Extend(context.Context, int, bool) (contracts.Snapshot, error) {
	return contracts.Snapshot{}, nil
}
func (c *scriptClient) AbortSleep(context.Context) (contracts.Snapshot, error) {
	return contracts.Snapshot{}, nil
}
func (c *scriptClient) Subscribe(context.Context) (<-chan contracts.Snapshot, error) {
	return nil, errors.New("no")
}

func notRunning() (contracts.Snapshot, error) {
	return contracts.Snapshot{}, fmt.Errorf("%w (test)", contracts.ErrEngineNotRunning)
}

// readyAfter is not running for the first n calls, up without a lease for
// one call, then ready.
func readyAfter(n int) *scriptClient {
	return &scriptClient{status: func(i int) (contracts.Snapshot, error) {
		switch {
		case i <= n:
			return notRunning()
		case i == n+1:
			s := runningSnap()
			s.Lease.Held = false
			return s, nil
		}
		return runningSnap(), nil
	}}
}

func TestWaitReadyPollsUntilLeaseHeld(t *testing.T) {
	c := readyAfter(3)
	snap, err := waitReady(context.Background(), c, make(chan error), time.Millisecond, time.Second)
	if err != nil || snap.PID != 4242 {
		t.Fatalf("snap=%+v err=%v", snap, err)
	}
	if c.calls != 5 {
		t.Fatalf("calls = %d, want 5", c.calls)
	}
}

func TestWaitReadyChildExits(t *testing.T) {
	c := &scriptClient{status: func(int) (contracts.Snapshot, error) { return notRunning() }}
	exited := make(chan error, 1)
	exited <- errors.New("exit status 1")
	start := time.Now()
	_, err := waitReady(context.Background(), c, exited, 50*time.Millisecond, 5*time.Second)
	if err == nil || !strings.Contains(err.Error(), "exited during startup") {
		t.Fatalf("err = %v", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("did not fail early")
	}
}

func TestWaitReadyTimeoutReportsLastError(t *testing.T) {
	c := &scriptClient{status: func(int) (contracts.Snapshot, error) {
		s := runningSnap()
		s.Lease.Held = false
		s.Errors = map[string]string{"lease": "backpackd unavailable"}
		return s, nil
	}}
	_, err := waitReady(context.Background(), c, make(chan error), time.Millisecond, 20*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "not ready") || !strings.Contains(err.Error(), "backpackd unavailable") {
		t.Fatalf("err = %v", err)
	}
}

func TestWaitReadySessionEnded(t *testing.T) {
	c := &scriptClient{status: func(int) (contracts.Snapshot, error) {
		s := runningSnap()
		s.Session.Ended, s.Session.EndReason = true, contracts.EndLeaseLost
		return s, nil
	}}
	_, err := waitReady(context.Background(), c, make(chan error), time.Millisecond, time.Second)
	if err == nil || !strings.Contains(err.Error(), "lease_lost") {
		t.Fatalf("err = %v", err)
	}
}

// withEngine wires a RunEngine that must not be called in-process.
func withEngine(te *testEnv) {
	te.wiring.RunEngine = func(context.Context, config.Config, EngineOptions) error {
		return errors.New("RunEngine must not run in the parent")
	}
}

func TestStartNonTTYAutoDetaches(t *testing.T) {
	te := newTestEnv(t)
	withEngine(te)
	client := readyAfter(2) // first call is the single-instance check
	te.engine = client
	var restored bool
	te.wiring.RestoreJournal = func(context.Context) (bool, error) { restored = true; return false, nil }
	te.spawn = func(args []string, logPath string) (*child, error) {
		te.spawned = append(te.spawned, args)
		if logPath != te.user.EngineLog {
			t.Errorf("log = %s", logPath)
		}
		return &child{pid: 4242, exited: make(chan error), logPath: logPath}, nil
	}
	if code := te.run("90", "--hotspot=on"); code != exitOK {
		t.Fatalf("code = %d, stderr %q", code, te.errb)
	}
	if got := te.out.String(); got != "backpack: awake until 23:40 (pid 4242)\n" {
		t.Fatalf("stdout = %q", got)
	}
	// Agents parse this: one line on stdout, nothing on stderr.
	if te.errb.Len() != 0 {
		t.Fatalf("stderr = %q", te.errb)
	}
	want := "_engine --sources duration=flag,hotspot=flag,watchdog=default --hotspot=on --watchdog 1h30m"
	if len(te.spawned) != 1 || strings.Join(te.spawned[0], " ") != want {
		t.Fatalf("spawned = %v", te.spawned)
	}
	if !restored {
		t.Error("stale journal restore not attempted before start")
	}
}

func TestStartExplicitDetachNoNote(t *testing.T) {
	te := newTestEnv(t)
	withEngine(te)
	te.engine = readyAfter(1)
	cfgPath := filepath.Join(t.TempDir(), "c.toml")
	if err := os.WriteFile(cfgPath, []byte("[session]\npoll = \"10s\"\n[notify]\nntfy_topic = \"t0pic\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	te.spawn = func(args []string, logPath string) (*child, error) {
		te.spawned = append(te.spawned, args)
		return &child{exited: make(chan error), logPath: logPath}, nil
	}
	if code := te.run("-d", "--config", cfgPath); code != exitOK {
		t.Fatalf("code = %d, stderr %q", code, te.errb)
	}
	if te.errb.Len() != 0 {
		t.Errorf("stderr = %q", te.errb)
	}
	if got := strings.Join(te.spawned[0], " "); got != "_engine --config "+cfgPath+" --sources duration=default,hotspot=default,watchdog=default --hotspot=auto --watchdog forever" {
		t.Fatalf("spawned = %q", got)
	}
}

func TestStartDetachFailureShowsLogTail(t *testing.T) {
	te := newTestEnv(t)
	withEngine(te)
	te.engine = &scriptClient{status: func(int) (contracts.Snapshot, error) { return notRunning() }}
	logPath := filepath.Join(t.TempDir(), "engine.log")
	if err := os.WriteFile(logPath, []byte("old run noise\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	te.spawn = func(args []string, _ string) (*child, error) {
		f, _ := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0)
		fmt.Fprintln(f, "engine: acquire: backpackd unavailable")
		f.Close()
		ex := make(chan error, 1)
		ex <- errors.New("exit status 1")
		return &child{exited: ex, logPath: logPath, logOffset: int64(len("old run noise\n"))}, nil
	}
	if code := te.run("-d", "30"); code != exitErr {
		t.Fatalf("code = %d", code)
	}
	errs := te.errb.String()
	if !strings.Contains(errs, "backpackd unavailable") || strings.Contains(errs, "old run noise") {
		t.Fatalf("stderr = %q", errs)
	}
	if te.out.Len() != 0 {
		t.Fatalf("stdout = %q", te.out)
	}
}

func TestStartAlreadyRunningNonTTYPrintsStatus(t *testing.T) {
	te := newTestEnv(t)
	withEngine(te)
	te.engine = fakes.NewEngineClient(runningSnap())
	te.spawn = func([]string, string) (*child, error) { t.Fatal("must not spawn"); return nil, nil }
	if code := te.run("90"); code != exitOK {
		t.Fatalf("code = %d", code)
	}
	if !strings.Contains(te.out.String(), "already running; awake until 23:40 (pid 4242)") {
		t.Fatalf("stdout = %q", te.out)
	}
	if !strings.Contains(te.errb.String(), "extend") {
		t.Fatalf("stderr = %q", te.errb)
	}
}

func TestStartAlreadyRunningTTYAttaches(t *testing.T) {
	te := newTestEnv(t)
	te.isTTY = func() bool { return true }
	te.engine = fakes.NewEngineClient(runningSnap())
	attached := false
	te.wiring.RunTUI = func(ctx context.Context, c contracts.EngineClient, _ TUIOptions) error {
		attached = c == te.engine
		return nil
	}
	if code := te.run(); code != exitOK || !attached {
		t.Fatalf("code=%d attached=%v", code, attached)
	}
}

func TestStartTTYDetachesThenAttaches(t *testing.T) {
	te := newTestEnv(t)
	withEngine(te)
	te.isTTY = func() bool { return true }
	te.engine = readyAfter(1)
	te.spawn = func(args []string, logPath string) (*child, error) {
		return &child{exited: make(chan error), logPath: logPath}, nil
	}
	attached := false
	te.wiring.RunTUI = func(context.Context, contracts.EngineClient, TUIOptions) error { attached = true; return nil }
	if code := te.run("2h", "-y"); code != exitOK || !attached {
		t.Fatalf("code=%d attached=%v stderr=%q", code, attached, te.errb)
	}
}

func TestStartForegroundRunsEngineInProcess(t *testing.T) {
	te := newTestEnv(t)
	var got EngineOptions
	var gotCfg config.Config
	te.wiring.RunEngine = func(ctx context.Context, cfg config.Config, opts EngineOptions) error {
		got, gotCfg = opts, cfg
		return nil
	}
	if code := te.run("-f", "--hotspot=off", "--no-watchdog", "45"); code != exitOK {
		t.Fatalf("code = %d, stderr %q", code, te.errb)
	}
	if got.Duration != 45*time.Minute || gotCfg.Network.Watchdog || gotCfg.Network.PreferHotspot != config.HotspotOff {
		t.Fatalf("opts=%+v watchdog=%v", got, gotCfg.Network.Watchdog)
	}
}

func TestStartForegroundEngineError(t *testing.T) {
	te := newTestEnv(t)
	te.wiring.RunEngine = func(context.Context, config.Config, EngineOptions) error { return contracts.ErrDaemonUnavailable }
	if code := te.run("--foreground"); code != exitErr {
		t.Fatalf("code = %d", code)
	}
	if !strings.Contains(te.errb.String(), "backpackd unavailable") {
		t.Fatalf("stderr = %q", te.errb)
	}
}

func TestStartForegroundTUIStopsEngineOnReturn(t *testing.T) {
	te := newTestEnv(t)
	te.isTTY = func() bool { return true }
	engineDone := make(chan struct{})
	up := make(chan struct{})
	te.wiring.RunEngine = func(ctx context.Context, _ config.Config, _ EngineOptions) error {
		close(up)
		<-ctx.Done()
		close(engineDone)
		return nil
	}
	te.engine = &scriptClient{status: func(n int) (contracts.Snapshot, error) {
		select {
		case <-up:
			return runningSnap(), nil
		default:
			return notRunning()
		}
	}}
	var fg bool
	te.wiring.RunTUI = func(_ context.Context, _ contracts.EngineClient, o TUIOptions) error { fg = o.Foreground; return nil }
	if code := te.run("-f", "--yes"); code != exitOK || !fg {
		t.Fatalf("code = %d, stderr %q", code, te.errb)
	}
	select {
	case <-engineDone:
	default:
		t.Fatal("engine not stopped after the TUI returned")
	}
}

func TestEngineSubcommandRunsEngine(t *testing.T) {
	te := newTestEnv(t)
	var got EngineOptions
	te.wiring.RunEngine = func(_ context.Context, _ config.Config, opts EngineOptions) error { got = opts; return nil }
	if code := te.run("_engine", "--sources", "duration=prompt,hotspot=flag", "--hotspot=on", "forever"); code != exitOK {
		t.Fatalf("code = %d, stderr %q", code, te.errb)
	}
	want := []contracts.SessionOption{
		{Name: "duration", Value: "forever", Source: "prompt"},
		{Name: "hotspot", Value: "on", Source: "flag"},
		{Name: "watchdog", Value: "on", Source: "default"},
	}
	if got.Duration != 0 || !slices.Equal(got.Choices, want) {
		t.Fatalf("opts = %+v", got)
	}
	for _, bad := range []string{"duration=typed", "speed=flag", "duration"} {
		te := newTestEnv(t)
		te.wiring.RunEngine = func(context.Context, config.Config, EngineOptions) error { return nil }
		if code := te.run("_engine", "--sources", bad); code != exitUsage {
			t.Errorf("--sources %s: code %d", bad, code)
		}
	}
	// _engine never takes -y (and so never prompts).
	if code := newTestEnv(t).run("_engine", "-y"); code != exitUsage {
		t.Errorf("_engine -y: code %d", code)
	}
}

// TestChildArgsRoundTrip: what the parent passes, the child parses back
// to the same values and sources.
func TestChildArgsRoundTrip(t *testing.T) {
	c := choices{
		"duration": {value: "1h30m", source: "prompt"},
		"hotspot":  {value: "off", source: "flag"},
		"watchdog": {value: "off", source: "default"},
	}
	args := childArgs(startOpts{choices: c}, "rel/c.toml")
	if args[0] != engineSubcommand || args[1] != "--config" || !filepath.IsAbs(args[2]) {
		t.Fatalf("args = %v", args)
	}
	if got := strings.Join(args[3:], " "); got != "--sources duration=prompt,hotspot=flag,watchdog=default --hotspot=off --no-watchdog 1h30m" {
		t.Fatalf("args = %q", got)
	}
	te := newTestEnv(t)
	var got EngineOptions
	te.wiring.RunEngine = func(_ context.Context, _ config.Config, opts EngineOptions) error { got = opts; return nil }
	if code := te.run(append(args[:1:1], args[3:]...)...); code != exitOK {
		t.Fatalf("child: %d %q", code, te.errb)
	}
	if !slices.Equal(got.Choices, c.list()) || got.Duration != 90*time.Minute {
		t.Fatalf("child got %+v, want %+v", got.Choices, c.list())
	}
}

func TestLogTail(t *testing.T) {
	p := filepath.Join(t.TempDir(), "log")
	var b strings.Builder
	for i := 1; i <= 30; i++ {
		fmt.Fprintf(&b, "line %d\n", i)
	}
	if err := os.WriteFile(p, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	tail := logTail(p, 0, 3)
	if tail != "line 28\nline 29\nline 30\n" {
		t.Fatalf("tail = %q", tail)
	}
	if logTail(p, int64(b.Len()), 3) != "" || logTail("/nonexistent/x", 0, 3) != "" {
		t.Fatal("expected empty tails")
	}
}

// TestHelperChild is the process spawnDetached starts in
// TestSpawnDetachedReal; it is a no-op in a normal test run.
func TestHelperChild(t *testing.T) {
	if os.Getenv("BACKPACK_TEST_CHILD") != "1" {
		t.Skip("helper process only")
	}
	sid, _ := unix.Getsid(0)
	buf := make([]byte, 8)
	n, err := os.Stdin.Read(buf)
	fmt.Printf("child sid_is_pid=%v stdin_n=%d stdin_err=%v tty=%v\n", sid == os.Getpid(), n, err, isTerminal(os.Stdin))
	fmt.Fprintln(os.Stderr, "child stderr line")
	os.Exit(0)
}

func TestSpawnDetachedReal(t *testing.T) {
	t.Setenv("BACKPACK_TEST_CHILD", "1")
	logPath := filepath.Join(t.TempDir(), "logs", "engine.log")
	ch, err := spawnDetached([]string{"-test.run=^TestHelperChild$"}, logPath)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-ch.exited:
		if err != nil {
			t.Fatalf("child: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("child did not exit")
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	log := string(data)
	for _, want := range []string{"=== ", "sid_is_pid=true", "stdin_n=0 stdin_err=EOF", "tty=false", "child stderr line"} {
		if !strings.Contains(log, want) {
			t.Errorf("log lacks %q:\n%s", want, log)
		}
	}
	fi, err := os.Stat(logPath)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("log mode = %v, %v", fi.Mode(), err)
	}
}
