package cli

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/ilias-t/bckp/internal/config"
	"github.com/ilias-t/bckp/internal/contracts"
	"github.com/ilias-t/bckp/internal/eventlog"
	"github.com/ilias-t/bckp/internal/humanize"
	"github.com/ilias-t/bckp/internal/paths"
)

// startOpts are the parsed `backpack [DUR]` arguments.
type startOpts struct {
	durArg     string // as given ("" for a bare `backpack`)
	detach     bool
	foreground bool
	noTUI      bool
	yes        bool
	configPath string
	// choices holds the session options given on the command line; the
	// rest are asked or defaulted (see options.go).
	choices choices
	// sources is the hidden `_engine --sources` value.
	sources string
}

// parseSessionDuration parses DUR: a bare integer is minutes ("90"),
// otherwise a Go duration ("2h", "1h30m"), or "forever". Sessions shorter
// than a minute are rejected.
func parseSessionDuration(s string) (time.Duration, bool, error) {
	s = strings.TrimSpace(s)
	if strings.EqualFold(s, "forever") {
		return 0, true, nil
	}
	var d time.Duration
	if n, err := strconv.Atoi(s); err == nil {
		if n <= 0 || n > 7*24*60 {
			return 0, false, fmt.Errorf("duration %q: minutes must be 1..%d", s, 7*24*60)
		}
		d = time.Duration(n) * time.Minute
	} else {
		pd, perr := time.ParseDuration(s)
		if perr != nil {
			return 0, false, fmt.Errorf("duration %q: want minutes (90), a duration (2h, 1h30m) or forever", s)
		}
		d = pd
	}
	if d < time.Minute {
		return 0, false, fmt.Errorf("duration %q: must be at least 1m", s)
	}
	return d, false, nil
}

func startFlags(e *env, name string, o *startOpts, optErr *error) *flagSet {
	fs := newFlags(e, name)
	fs.BoolVar(&o.detach, "d", false, "run in the background and return")
	fs.BoolVar(&o.detach, "detach", false, "run in the background and return")
	fs.BoolVar(&o.foreground, "f", false, "run the engine in this process")
	fs.BoolVar(&o.foreground, "foreground", false, "run the engine in this process")
	fs.BoolVar(&o.noTUI, "no-tui", false, "don't open the TUI")
	fs.StringVar(&o.configPath, "config", "", "read the config from `PATH`")
	if name == engineSubcommand {
		// The detached child is never asked anything; the parent passes
		// where each option came from.
		fs.StringVar(&o.sources, "sources", "", "option sources (internal)")
	} else {
		fs.BoolVar(&o.yes, "y", false, "don't ask; take the defaults")
		fs.BoolVar(&o.yes, "yes", false, "don't ask; take the defaults")
	}
	registerOptionFlags(fs.FlagSet, o.choices, optErr)
	return fs
}

// parseStart parses start (and _engine) arguments.
func parseStart(e *env, name string, args []string) (startOpts, int, bool) {
	o := startOpts{choices: choices{}}
	var optErr error
	fs := startFlags(e, name, &o, &optErr)
	pos, code, ok := e.parseArgs(fs, args)
	if !ok {
		return o, code, false
	}
	if optErr != nil {
		e.errorf("%v", optErr)
		return o, exitUsage, false
	}
	switch len(pos) {
	case 0:
	case 1:
		if err := o.choices.setArg(pos[0]); err != nil {
			e.errorf("%v", err)
			return o, exitUsage, false
		}
		o.durArg = pos[0]
	default:
		e.errorf("too many arguments: %s", strings.Join(pos, " "))
		return o, exitUsage, false
	}
	return o, 0, true
}

// startMode says how a new session runs.
type startMode int

const (
	// modeDetach spawns a detached engine, waits for it, prints one line
	// and returns.
	modeDetach startMode = iota
	// modeDetachTUI spawns a detached engine and attaches the TUI; `q`
	// leaves the engine running.
	modeDetachTUI
	// modeForeground runs the engine in this process without a TUI.
	modeForeground
	// modeForegroundTUI runs the engine in this process with the TUI;
	// leaving the TUI ends the session.
	modeForegroundTUI
)

// decideStartMode picks the mode from the flags and whether stdin and
// stdout are terminals. A non-TTY call without --foreground always
// detaches, so a caller without a terminal (an agent's shell tool) never
// hangs.
func decideStartMode(o startOpts, tty bool) (startMode, error) {
	switch {
	case o.detach && o.foreground:
		return 0, errors.New("--detach and --foreground conflict")
	case o.foreground:
		if tty && !o.noTUI {
			return modeForegroundTUI, nil
		}
		return modeForeground, nil
	case o.detach, !tty, o.noTUI:
		return modeDetach, nil
	default:
		return modeDetachTUI, nil
	}
}

func cmdStart(e *env, args []string) int {
	o, code, ok := parseStart(e, "", args)
	if !ok {
		return code
	}
	cfg, cfgPath, err := e.loadConfig(o.configPath)
	if err != nil {
		e.errorf("%v", err)
		return exitErr
	}
	tty := e.isTTY()
	mode, err := decideStartMode(o, tty)
	if err != nil {
		e.errorf("%v", err)
		return exitUsage
	}

	// Single instance: a running engine wins.
	client := e.client()
	ctx, cancel := shortCtx()
	snap, err := client.Status(ctx)
	cancel()
	if err == nil {
		return alreadyRunning(e, o, snap, client, tty)
	}
	if !errors.Is(err, contracts.ErrEngineNotRunning) {
		e.errorf("checking for a running engine: %v", err)
		return exitErr
	}
	if e.wiring.RunEngine == nil {
		return e.notWired("the engine")
	}

	// Prompts only in a terminal and without --yes; without a terminal
	// nothing reads stdin, so a caller like an agent's shell never blocks.
	ask := tty && !o.yes
	if code := e.ensureDaemon(ask); code != exitOK {
		return code
	}
	askedTopic := false
	if ask && o.configPath == "" {
		if _, explicit := config.ResolvePath("", e.environ, e.user); !explicit {
			var code int
			if askedTopic, code = e.firstRun(cfgPath); code != exitOK {
				return code
			}
			if cfg, _, err = e.loadConfig(o.configPath); err != nil {
				e.errorf("%v", err)
				return exitErr
			}
		}
	}
	if cfg, code = e.ensureTopic(cfg, cfgPath, ask && !askedTopic); code != exitOK {
		return code
	}
	if ask {
		_, hotspotGiven := o.choices[contracts.OptionHotspot]
		haveHotspot := hotspotGiven || e.hasService(cfg.Network.Service)
		if err := askOptions(e.prompter(), o.choices, cfg, haveHotspot); err != nil {
			e.errorf("%v", err)
			return exitErr
		}
	}
	o.choices.fillDefaults(cfg)
	if tty {
		fmt.Fprintf(e.stdout, "%s: %s\n", e.prog, choicesSummary(o.choices, cfg))
	}

	e.restoreStaleJournal()

	opts, err := o.choices.engineOptions(&cfg)
	if err != nil {
		e.errorf("%v", err)
		return exitUsage
	}
	switch mode {
	case modeForeground:
		return runForeground(e, cfg, opts, false)
	case modeForegroundTUI:
		if e.wiring.RunTUI == nil {
			e.errorf("the TUI is not wired in this build; running without it")
			return runForeground(e, cfg, opts, false)
		}
		return runForeground(e, cfg, opts, true)
	}

	snap, pid, code := detach(e, o)
	e.logDetach(cfg, o, args, mode, tty, pid, code)
	if code != exitOK {
		return code
	}
	if mode == modeDetachTUI && e.wiring.RunTUI != nil {
		return attachTUI(e, client)
	}
	fmt.Fprintf(e.stdout, "%s: %s\n", e.prog, readyLine(snap, cfg, e.now()))
	return exitOK
}

// hasService reports whether the network service exists (`networksetup
// -listallnetworkservices`; a leading "*" marks a disabled one). When the
// list can't be read it answers true, so the question is still asked.
func (e *env) hasService(name string) bool {
	ctx, cancel := shortCtx()
	defer cancel()
	res, err := e.exec.Run(ctx, paths.BinNetworksetup, "-listallnetworkservices")
	if err != nil {
		return true
	}
	for _, l := range strings.Split(string(res.Stdout), "\n") {
		if strings.TrimPrefix(strings.TrimSpace(l), "*") == name {
			return true
		}
	}
	return false
}

// logDetach records session.detach (src=cli) in cli.jsonl: who started
// the session and how, so `backpack logs` can tell a phone- or
// agent-started session from one started at the keyboard.
func (e *env) logDetach(cfg config.Config, o startOpts, args []string, mode startMode, tty bool, pid, code int) {
	log, closeLog := e.cliLog(cfg)
	defer closeLog()
	how := "detach"
	switch {
	case mode == modeDetachTUI:
		how = "tui"
	case !tty && !o.detach:
		how = "auto-detach"
	}
	choices := map[string]string{}
	for _, so := range o.choices.list() {
		choices[so.Name] = so.Value + " (" + so.Source + ")"
	}
	fields := []any{
		"mode", how, "tty", tty, "yes", o.yes, "argv", args, "ppid", os.Getppid(),
		"ssh", getenvSet(e.environ, "SSH_CONNECTION"), "choices", choices,
	}
	if tp, ok := config.LookupEnv(e.environ, "TERM_PROGRAM"); ok {
		fields = append(fields, "term", tp)
	}
	level := slog.LevelInfo
	if code == exitOK {
		fields = append(fields, "pid", pid, "result", "ok")
	} else {
		level = slog.LevelError
		fields = append(fields, "result", "failed")
	}
	eventlog.Log(log, level, "session.detach", fields...)
}

func getenvSet(environ []string, key string) bool {
	v, ok := config.LookupEnv(environ, key)
	return ok && v != ""
}

// alreadyRunning handles `backpack [DUR]` while an engine runs: a
// terminal attaches the TUI, anything else gets a status line.
func alreadyRunning(e *env, o startOpts, snap contracts.Snapshot, client contracts.EngineClient, tty bool) int {
	if o.durArg != "" {
		e.errorf("already running (pid %d); use `%s extend` to change the time", snap.PID, e.prog)
	}
	if tty && !o.noTUI && !o.detach && e.wiring.RunTUI != nil {
		return attachTUI(e, client)
	}
	fmt.Fprintf(e.stdout, "%s: already running; %s\n", e.prog, readyLine(snap, config.Default(), e.now()))
	return exitOK
}

// attachTUI shows the TUI on a detached engine. An external signal
// (SIGINT/SIGTERM/SIGHUP) detaches like q: the engine keeps running.
func attachTUI(e *env, client contracts.EngineClient) int {
	ctx, cancel := e.signalContext(context.Background())
	defer cancel()
	if err := e.wiring.RunTUI(ctx, client, TUIOptions{}); err != nil {
		if ctx.Err() != nil {
			// Interrupted: the TUI normally reports this as a detach
			// itself; an error here still means the engine is untouched.
			fmt.Fprintf(e.stdout, "still running — `%s attach` to return\n", e.prog)
			return exitOK
		}
		e.errorf("tui: %v", err)
		return exitErr
	}
	return exitOK
}

// restoreStaleJournal re-applies a network restore journal left by a
// crashed session. Only called when no engine runs.
func (e *env) restoreStaleJournal() {
	if e.wiring.RestoreJournal == nil {
		return
	}
	ctx, cancel := shortCtx()
	defer cancel()
	restored, err := e.wiring.RestoreJournal(ctx)
	switch {
	case err != nil:
		e.errorf("restoring the network from a stale journal failed: %v", err)
	case restored:
		e.errorf("restored network settings left behind by an earlier session")
	}
}

// runForeground runs the engine in this process. With the TUI, leaving
// the TUI ends the session.
func runForeground(e *env, cfg config.Config, opts EngineOptions, withTUI bool) int {
	ctx, cancel := e.signalContext(context.Background())
	defer cancel()
	if !withTUI {
		if err := e.wiring.RunEngine(ctx, cfg, opts); err != nil {
			e.errorf("engine: %v", err)
			return exitErr
		}
		return exitOK
	}

	engCtx, stopEngine := context.WithCancel(ctx)
	defer stopEngine()
	exited := make(chan error, 1)
	go func() { exited <- e.wiring.RunEngine(engCtx, cfg, opts) }()

	client := e.client()
	if _, err := waitReady(ctx, client, exited, e.pollInterval, e.readyTimeout); err != nil {
		stopEngine()
		e.errorf("%v", err)
		return exitErr
	}
	tuiErr := e.wiring.RunTUI(ctx, client, TUIOptions{Foreground: true})
	stopEngine()
	engErr := <-exited
	switch {
	case tuiErr != nil && ctx.Err() == nil:
		e.errorf("tui: %v", tuiErr)
		return exitErr
	case engErr != nil:
		e.errorf("engine: %v", engErr)
		return exitErr
	}
	return exitOK
}

// child is a spawned detached engine.
type child struct {
	pid int
	// exited receives the child's exit (nil or the wait error) if it ends.
	exited <-chan error
	// logPath and logOffset locate this run's output in the engine log.
	logPath   string
	logOffset int64
}

// childArgs builds the detached child's argv (after the executable):
// `_engine`, an absolute config path (the child runs from "/"), every
// session option as a flag with its resolved value, their sources, and
// the duration. --yes is never passed: the child never asks anything.
func childArgs(o startOpts, cfgPath string) []string {
	args := []string{engineSubcommand}
	if cfgPath != "" {
		if abs, err := filepath.Abs(cfgPath); err == nil {
			cfgPath = abs
		}
		args = append(args, "--config", cfgPath)
	}
	args = append(args, "--sources", o.choices.sourcesArg())
	return append(args, o.choices.childFlags()...)
}

// detach spawns the engine child and waits for it to hold the lease. pid
// is the child's (0 if it never started).
func detach(e *env, o startOpts) (contracts.Snapshot, int, int) {
	// Pass the config path only when the user chose it; the child resolves
	// the default (and BACKPACK_CONFIG) itself otherwise.
	explicitPath := ""
	if p, explicit := config.ResolvePath(o.configPath, e.environ, e.user); explicit {
		explicitPath = p
	}
	ch, err := e.spawn(childArgs(o, explicitPath), e.user.EngineLog)
	if err != nil {
		e.errorf("starting the engine: %v", err)
		return contracts.Snapshot{}, 0, exitErr
	}
	snap, err := waitReady(context.Background(), e.client(), ch.exited, e.pollInterval, e.readyTimeout)
	if err != nil {
		e.errorf("%v", err)
		if tail := logTail(ch.logPath, ch.logOffset, 20); tail != "" {
			fmt.Fprintf(e.stderr, "--- last lines of %s ---\n%s", ch.logPath, tail)
		} else {
			fmt.Fprintf(e.stderr, "(nothing in %s)\n", ch.logPath)
		}
		return contracts.Snapshot{}, ch.pid, exitErr
	}
	return snap, ch.pid, exitOK
}

// waitReady polls Status every interval until the engine holds its
// lease, the child exits, or timeout passes.
func waitReady(ctx context.Context, c contracts.EngineClient, exited <-chan error, interval, timeout time.Duration) (contracts.Snapshot, error) {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	tick := time.NewTicker(interval)
	defer tick.Stop()
	var lastErr error
	for {
		pctx, cancel := context.WithTimeout(ctx, max(interval*5, time.Second))
		snap, err := c.Status(pctx)
		cancel()
		switch {
		case err == nil && snap.Session.Ended:
			return snap, fmt.Errorf("the session ended during startup (%s)", snap.Session.EndReason)
		case err == nil && snap.Lease.Held:
			return snap, nil
		case err == nil:
			lastErr = errors.New("engine is up but has not acquired the sleep lease")
			if msg := snap.Errors["lease"]; msg != "" {
				lastErr = fmt.Errorf("engine is up but has no sleep lease: %s", msg)
			}
		default:
			lastErr = err
		}
		select {
		case werr := <-exited:
			if werr != nil {
				return contracts.Snapshot{}, fmt.Errorf("the engine exited during startup: %v", werr)
			}
			return contracts.Snapshot{}, errors.New("the engine exited during startup")
		case <-deadline.C:
			return contracts.Snapshot{}, fmt.Errorf("the engine was not ready after %s: %v", timeout, lastErr)
		case <-ctx.Done():
			return contracts.Snapshot{}, ctx.Err()
		case <-tick.C:
		}
	}
}

// spawnDetached re-execs this binary as a session leader with stdin from
// /dev/null and stdout/stderr appended to logPath. Only fds 0-2 are
// inherited (Go opens everything else close-on-exec).
func spawnDetached(args []string, logPath string) (*child, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		return nil, err
	}
	logf, err := os.OpenFile(logPath, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	defer logf.Close()
	var offset int64
	if fi, err := logf.Stat(); err == nil {
		offset = fi.Size()
	}
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		return nil, err
	}
	defer devnull.Close()
	fmt.Fprintf(logf, "=== %s: starting %s ===\n", time.Now().Format(time.RFC3339), strings.Join(args, " "))

	cmd := exec.Command(exe, args...)
	cmd.Dir = "/"
	cmd.Stdin = devnull
	cmd.Stdout = logf
	cmd.Stderr = logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	return &child{pid: cmd.Process.Pid, exited: exited, logPath: logPath, logOffset: offset}, nil
}

// logTail returns up to n trailing lines written to path after offset.
func logTail(path string, offset int64, n int) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return ""
	}
	// Bound what we read: the tail of at most 64 KiB.
	data, err := io.ReadAll(io.LimitReader(f, 1<<20))
	if err != nil {
		return ""
	}
	if len(data) > 64<<10 {
		data = data[len(data)-64<<10:]
	}
	var lines []string
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64<<10), 64<<10)
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\n") + "\n"
}

// cmdEngine is the hidden `_engine` subcommand run by the detached child.
func cmdEngine(e *env, args []string) int {
	o, code, ok := parseStart(e, engineSubcommand, args)
	if !ok {
		return code
	}
	if e.wiring.RunEngine == nil {
		return e.notWired("the engine")
	}
	cfg, _, err := e.loadConfig(o.configPath)
	if err != nil {
		e.errorf("%v", err)
		return exitErr
	}
	o.choices.fillDefaults(cfg)
	if err := o.choices.applySources(o.sources); err != nil {
		e.errorf("%v", err)
		return exitUsage
	}
	opts, err := o.choices.engineOptions(&cfg)
	if err != nil {
		e.errorf("%v", err)
		return exitUsage
	}
	fmt.Fprintf(e.stderr, "%s: engine pid %d starting (%s)\n", e.prog, os.Getpid(), optionsStatus(opts.Choices))
	return runForeground(e, cfg, opts, false)
}

// readyLine is the one-line session summary, with every session option
// the engine reports:
//
//	awake until 23:40 · hotspot auto · watchdog on (pid 4242)
//	awake until 11:40 (12h cap) · hotspot auto · watchdog on (pid 4242)
//	awake forever (12h cap at 11:40) · hotspot auto · watchdog on (pid 4242)
func readyLine(s contracts.Snapshot, cfg config.Config, now time.Time) string {
	ss := s.Session
	capDur := cfg.Session.MaxDuration
	if !ss.CapAt.IsZero() && !ss.StartedAt.IsZero() && ss.CapAt.After(ss.StartedAt) {
		capDur = ss.CapAt.Sub(ss.StartedAt)
	}
	opts := describeOptions(s.Options) + fmt.Sprintf(" (pid %d)", s.PID)
	if ss.Forever || ss.EndsAt.IsZero() {
		if ss.CapAt.IsZero() {
			return "awake forever" + opts
		}
		return fmt.Sprintf("awake forever (%s cap at %s)", humanize.Duration(capDur), humanize.Clock(ss.CapAt, now)) + opts
	}
	end := ss.EndsAt
	capped := ""
	if !ss.CapAt.IsZero() && ss.CapAt.Before(end) {
		end = ss.CapAt
		capped = fmt.Sprintf(" (%s cap)", humanize.Duration(capDur))
	}
	return fmt.Sprintf("awake until %s%s", humanize.Clock(end, now), capped) + opts
}
