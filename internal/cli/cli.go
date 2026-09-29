// Package cli implements the backpack command line (`backpack`, alias
// `bckp`).
//
// The CLI must not import the engine, daemon, provider or notifier
// packages; those are injected through Wiring by cmd/backpack. Everything
// that touches the OS (TTY detection, exec, uid, filesystem ownership,
// process spawning) goes through env so tests can fake it.
package cli

import (
	"context"
	"crypto/rand"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/ilias-t/bckp/internal/config"
	"github.com/ilias-t/bckp/internal/contracts"
	"github.com/ilias-t/bckp/internal/engineclient"
	"github.com/ilias-t/bckp/internal/paths"
	"github.com/ilias-t/bckp/internal/proto"
	"github.com/ilias-t/bckp/internal/system"
	"github.com/ilias-t/bckp/internal/version"
)

// Exit codes.
const (
	exitOK         = 0
	exitErr        = 1
	exitUsage      = 2
	exitNotRunning = 3
)

// EngineOptions are the per-session options passed to Wiring.RunEngine.
// The hotspot preference and the watchdog are applied to the config passed
// alongside (Network.PreferHotspot, Network.Watchdog).
type EngineOptions struct {
	// Duration is the requested session length; zero means forever (still
	// capped by session.max_duration).
	Duration time.Duration
	// Choices are every session option with its source, for
	// Snapshot.Options and `status`.
	Choices []contracts.SessionOption
}

// TUIOptions configure Wiring.RunTUI.
type TUIOptions struct {
	// Foreground: the engine runs in this process (`-f`), so q asks to
	// stop the session instead of detaching.
	Foreground bool
}

// NamedNotifier is one notification backend ("ntfy", "macOS", "log").
type NamedNotifier struct {
	Name string
	contracts.Notifier
}

// Wiring injects the implementations of the engine, daemon client,
// providers, notifiers and TUI, which the CLI does not import. Any nil
// field makes the commands that need it fail with "not wired in this
// build".
type Wiring struct {
	// NewGuard returns a client for backpackd (used by doctor and the
	// start for the daemon status check).
	NewGuard func() contracts.SleepGuard
	// RunEngine runs the engine and serves the engine protocol on
	// paths.User.EngineSocket until the session ends or ctx is cancelled
	// (SIGINT/SIGTERM/SIGHUP cancel it). It must acquire the daemon lease
	// before reporting Lease.Held in its snapshots. A startup failure is
	// returned as an error (the detached child logs it and exits 1).
	RunEngine func(ctx context.Context, cfg config.Config, opts EngineOptions) error
	// RestoreJournal re-applies a stale network restore journal. It
	// reports whether a journal was found and restored. Only called when
	// no engine is running.
	RestoreJournal func(ctx context.Context) (bool, error)
	// Simulate runs `backpack simulate` with fake clock and providers,
	// writing its report to out. It never sends notifications or sleeps. A
	// script that does not parse is an error wrapping
	// contracts.ErrBadScript.
	Simulate func(ctx context.Context, script string, cfg config.Config, out io.Writer) error
	// SimulateGrammar describes simulate's script language, for
	// `simulate --help` and bad scripts.
	SimulateGrammar string
	// NewNotifiers builds the configured notification backends, each on
	// its own so `notify test` can report them one by one. Send must return only after the backend accepted the message
	// (ntfy HTTP 2xx), bypassing the outbox. The log backend writes to
	// log.
	NewNotifiers func(cfg config.Config, log *slog.Logger) []NamedNotifier
	// DaemonPlist renders the LaunchDaemon plist for paths.DaemonBinary.
	DaemonPlist func() []byte
	// RunTUI runs the TUI against a running engine until the user detaches
	// (q), the session ends, or ctx is cancelled (an external signal); it
	// returns nil in all three cases, and prints the "still running" line
	// itself when not in the foreground. On Ctrl-C the TUI itself calls
	// client.Stop(ctx, nil). The CLI only calls it when stdin and stdout
	// are terminals.
	RunTUI func(ctx context.Context, client contracts.EngineClient, o TUIOptions) error
}

// env is everything the CLI takes from the OS. Main fills the real one.
type env struct {
	prog    string
	stdin   io.Reader
	stdout  io.Writer
	stderr  io.Writer
	environ []string
	wiring  Wiring

	// isTTY reports whether stdin and stdout are both terminals.
	isTTY func() bool
	exec  contracts.Exec
	user  paths.User
	// userName is the login name of the invoking user (for dseditgroup).
	userName string
	// root prefixes every system path (paths.Daemon*, AllowedUIDs,
	// MacmonCandidates). "" in production; a temp dir in tests.
	root   string
	getuid func() int
	// executable returns the running binary's path (os.Executable).
	executable func() (string, error)
	chown      func(path string, uid, gid int) error
	// ownerOf returns the owner uid/gid of a stat result.
	ownerOf   func(fi os.FileInfo) (uid, gid int)
	newClient func(socket string) contracts.EngineClient
	// spawn starts the detached engine child.
	spawn func(args []string, logPath string) (*child, error)
	now   func() time.Time
	sleep func(time.Duration)
	// signalContext returns a context cancelled on SIGINT/SIGTERM/SIGHUP.
	signalContext func(ctx context.Context) (context.Context, context.CancelFunc)
	// pollInterval and readyTimeout drive detach readiness.
	pollInterval time.Duration
	readyTimeout time.Duration
	// interactive runs a command with the terminal attached (the sudo
	// install offer).
	interactive func(ctx context.Context, name string, args ...string) error
	// random is the entropy for generated ntfy topics.
	random io.Reader
	// prompt is the run's shared prompter (see prompter()).
	prompt *prompter
}

// Main runs the CLI with args (os.Args, including the program name) and
// returns the process exit code.
func Main(args []string, w Wiring) int {
	e := realEnv(w)
	if len(args) > 0 {
		e.prog = progName(args[0])
		args = args[1:]
	}
	return run(e, args)
}

func realEnv(w Wiring) *env {
	u, err := paths.Current()
	if err != nil {
		u = paths.ForHome(os.Getenv("HOME"))
	}
	name := os.Getenv("SUDO_USER")
	if name == "" {
		name = os.Getenv("USER")
	}
	return &env{
		prog:       "backpack",
		stdin:      os.Stdin,
		stdout:     os.Stdout,
		stderr:     os.Stderr,
		environ:    os.Environ(),
		wiring:     w,
		isTTY:      func() bool { return isTerminal(os.Stdin) && isTerminal(os.Stdout) },
		exec:       system.Exec{},
		user:       u,
		userName:   name,
		getuid:     os.Getuid,
		executable: os.Executable,
		chown:      os.Chown,
		ownerOf:    statOwner,
		newClient:  engineclient.New,
		spawn:      spawnDetached,
		now:        time.Now,
		sleep:      time.Sleep,
		signalContext: func(ctx context.Context) (context.Context, context.CancelFunc) {
			return signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
		},
		pollInterval: proto.DetachPollInterval,
		readyTimeout: proto.DetachReadyTimeout,
		interactive:  runInteractive,
		random:       rand.Reader,
	}
}

func statOwner(fi os.FileInfo) (int, int) {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return int(st.Uid), int(st.Gid)
	}
	return -1, -1
}

// progName is argv[0]'s basename: "backpack" or "bckp".
func progName(argv0 string) string {
	b := filepath.Base(argv0)
	if b == "" || b == "." || b == "/" {
		return "backpack"
	}
	return b
}

// sys maps a system path under the root prefix.
func (e *env) sys(p string) string {
	if e.root == "" {
		return p
	}
	return filepath.Join(e.root, p)
}

func (e *env) errorf(format string, args ...any) {
	fmt.Fprintf(e.stderr, e.prog+": "+format+"\n", args...)
}

func (e *env) printf(format string, args ...any) {
	fmt.Fprintf(e.stdout, format, args...)
}

// command is one subcommand. Help and each command's usage line come from
// this table.
type command struct {
	name    string
	args    string // usage after the name, e.g. "[--json]"
	summary string
	run     func(e *env, args []string) int
	hidden  bool
}

func commands() []command {
	return []command{
		{name: "status", args: "[--json]", summary: "show the session (exit 3 when none is running)", run: cmdStatus},
		{name: "stop", args: "[--sleep|--no-sleep]", summary: "end the session", run: cmdStop},
		{name: "extend", args: "N|DUR|forever", summary: "add time (N is minutes); restarts the cap", run: cmdExtend},
		{name: "abort", summary: "cancel a pending heat sleep (adds no time)", run: cmdAbort},
		{name: "attach", summary: "reopen the dashboard", run: cmdAttach},
		{name: "logs", args: "[--since 2h] [-f]", summary: "what happened, with macOS's sleep/wake log", run: cmdLogs},
		{name: "doctor", args: "[--fix]", summary: "health checks", run: cmdDoctor},
		{name: "notify", args: "test", summary: "send a test alert to every backend", run: cmdNotify},
		{name: "config", args: "init|path|show", summary: "the config file", run: cmdConfig},
		{name: "simulate", args: `"SCRIPT"`, summary: "dry-run the safety rules on scripted readings", run: cmdSimulate},
		{name: "install", summary: "set up the root helper (run with sudo)", run: cmdInstall},
		{name: "uninstall", summary: "remove the root helper (run with sudo)", run: cmdUninstall},
		{name: "version", summary: "print the version", run: cmdVersion},
		{name: "help", summary: "show this help", run: cmdHelp},
		{name: engineSubcommand, run: cmdEngine, hidden: true},
	}
}

func commandNamed(name string) (command, bool) {
	for _, c := range commands() {
		if c.name == name {
			return c, true
		}
	}
	return command{}, false
}

// engineSubcommand is the hidden subcommand the detached child runs.
const engineSubcommand = "_engine"

func run(e *env, args []string) int {
	if len(args) > 0 {
		if c, ok := commandNamed(args[0]); ok {
			return c.run(e, args[1:])
		}
		switch args[0] {
		case "-h", "-help", "--help":
			return cmdHelp(e, nil)
		case "-v", "-version", "--version":
			return cmdVersion(e, nil)
		}
		// A bare word that is neither a command nor a duration is a typo.
		if !strings.HasPrefix(args[0], "-") {
			if _, _, err := parseSessionDuration(args[0]); err != nil {
				e.errorf("unknown command or duration %q (see `%s help`)", args[0], e.prog)
				return exitUsage
			}
		}
	}
	return cmdStart(e, args)
}

// helpCol is where descriptions start in help output.
const helpCol = 30

// helpRow prints "  left   right", with right starting at helpCol and
// wrapped at 80 columns.
func helpRow(w io.Writer, left, right string) {
	pad := strings.Repeat(" ", helpCol)
	line := "  " + left
	if len(line) >= helpCol-1 {
		fmt.Fprintln(w, line)
		line = pad
	} else {
		line += pad[len(line):]
	}
	width := len(line)
	for i, word := range strings.Fields(right) {
		if i > 0 && width+1+len(word) > 80 {
			fmt.Fprintln(w, line)
			line, width = pad+word, helpCol+len(word)
			continue
		}
		if i > 0 {
			line += " "
			width++
		}
		line += word
		width += len(word)
	}
	fmt.Fprintln(w, line)
}

func cmdHelp(e *env, _ []string) int {
	w, p := e.stdout, e.prog
	fmt.Fprintf(w, `%[1]s keeps a MacBook awake with the lid closed, watches heat and battery,
and puts it to sleep before it gets too hot.

Usage:
`, p)
	helpRow(w, p+" [DUR] [flags]", "start a session. DUR: 90 (minutes), 2h, 1h30m or forever (the default; every session is capped at session.max_duration, 12h)")
	fmt.Fprintf(w, "\nFlags:\n")
	helpRow(w, "-y, --yes", "don't ask; take the defaults")
	helpRow(w, "-d, --detach", "start in the background and return")
	helpRow(w, "-f, --foreground", "run in this terminal; ending it ends the session")
	helpRow(w, "    --no-tui", "don't open the dashboard")
	helpRow(w, "    --hotspot=auto|on|off", "prefer the iPhone hotspot over Wi-Fi (auto: once the phone is connected)")
	helpRow(w, "    --[no-]watchdog", "watch the network: outage alerts, hotspot recovery")
	helpRow(w, "    --config PATH", "read the config from PATH")
	fmt.Fprintf(w, "\nCommands:\n")
	for _, c := range commands() {
		if c.hidden || c.name == "help" {
			continue
		}
		helpRow(w, strings.TrimSpace(c.name+" "+c.args), c.summary)
	}
	fmt.Fprintf(w, `
In a terminal, options not given as flags are asked (Enter takes the
default). Without a terminal (ssh host %[1]s 2h, an agent's shell) nothing
is asked: the session starts in the background and %[1]s prints one line.
Run `+"`%[1]s COMMAND --help`"+` for a command's flags.
`, p)
	return exitOK
}

func cmdVersion(e *env, args []string) int {
	fs := newFlags(e, "version")
	pos, code, ok := e.parseArgs(fs, args)
	if !ok {
		return code
	}
	if len(pos) > 0 {
		e.errorf("version takes no arguments")
		return exitUsage
	}
	e.printf("%s %s\n", e.prog, version.String())
	return exitOK
}

// flagSet is a subcommand's flags. It never prints on its own and never
// exits; parseArgs reports errors and help.
type flagSet struct {
	*flag.FlagSet
	e    *env
	name string
	// extra are more flag rows (flags parsed by hand), as {left, usage}.
	extra [][2]string
	// notes is printed at the end of --help.
	notes string
}

// newFlags returns the flags for the command name; its usage line comes
// from commands(). A flag with an empty usage is hidden from --help.
func newFlags(e *env, name string) *flagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	return &flagSet{FlagSet: fs, e: e, name: name}
}

// usageLine is "usage: bckp status [--json]".
func (fs *flagSet) usageLine() string {
	c, ok := commandNamed(fs.name)
	if !ok {
		return fmt.Sprintf("usage: %s [DUR] [flags]", fs.e.prog)
	}
	return strings.TrimSpace(fmt.Sprintf("usage: %s %s %s", fs.e.prog, fs.name, c.args))
}

// printHelp writes the usage line, the summary and the flags, with a
// one-letter alias shown next to the long flag it shares a usage with
// ("-f, --follow").
func (fs *flagSet) printHelp(w io.Writer) {
	c, ok := commandNamed(fs.name)
	if !ok || c.hidden {
		cmdHelp(fs.e, nil) // starting a session: the main help
		return
	}
	fmt.Fprintf(w, "%s\n", fs.usageLine())
	if c.summary != "" {
		fmt.Fprintf(w, "\n%s.\n", strings.ToUpper(c.summary[:1])+c.summary[1:])
	}
	var long []*flag.Flag
	short := map[string]string{} // usage -> "-f"
	fs.VisitAll(func(f *flag.Flag) {
		switch {
		case f.Usage == "":
		case len(f.Name) == 1:
			short[f.Usage] = "-" + f.Name
		default:
			long = append(long, f)
		}
	})
	if len(long) > 0 {
		fmt.Fprintf(w, "\nFlags:\n")
	}
	for _, f := range long {
		left := "    --" + f.Name
		if s, ok := short[f.Usage]; ok {
			left = s + ", --" + f.Name
		}
		arg, usage := flag.UnquoteUsage(f)
		if arg != "" {
			left += " " + arg
		}
		if f.DefValue != "" && f.DefValue != "false" {
			usage += " (default " + f.DefValue + ")"
		}
		helpRow(w, left, usage)
	}
	for _, r := range fs.extra {
		helpRow(w, r[0], r[1])
	}
	if fs.notes != "" {
		fmt.Fprintf(w, "\n%s", fs.notes)
	}
}

// parseInterspersed parses flags that may appear before or after
// positional arguments ("90 -d" as well as "-d 90"). "--" ends flags.
func parseInterspersed(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return pos, nil
		}
		// flag stops at "--" (consumed) or the first non-flag.
		consumed := len(args) - len(rest)
		if consumed > 0 && args[consumed-1] == "--" {
			return append(pos, rest...), nil
		}
		pos = append(pos, rest[0])
		args = rest[1:]
	}
}

// parseArgs parses args: -h/--help prints the command's help on stdout
// (exit 0), a bad flag prints the error and the usage line on stderr (exit
// 2). ok is false when the caller should return code.
func (e *env) parseArgs(fs *flagSet, args []string) (pos []string, code int, ok bool) {
	pos, err := parseInterspersed(fs.FlagSet, args)
	switch {
	case errors.Is(err, flag.ErrHelp):
		fs.printHelp(e.stdout)
		return nil, exitOK, false
	case err != nil:
		e.errorf("%v", err)
		fmt.Fprintf(e.stderr, "%s (see --help)\n", fs.usageLine())
		return nil, exitUsage, false
	}
	return pos, 0, true
}

// loadConfig resolves and loads the config.
func (e *env) loadConfig(flagPath string) (config.Config, string, error) {
	path, explicit := config.ResolvePath(flagPath, e.environ, e.user)
	cfg, err := config.Load(config.LoadOptions{Path: path, MustExist: explicit, Environ: e.environ})
	return cfg, path, err
}

func (e *env) notWired(what string) int {
	e.errorf("%s is not wired in this build", what)
	return exitErr
}

func (e *env) client() contracts.EngineClient {
	return e.newClient(e.user.EngineSocket)
}

// shortCtx is a context for one quick request.
func shortCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 5*time.Second)
}

// opCtx is a context for an engine operation (abort, extend) that the
// engine handles between polls; a poll can take ~14s when offline.
func opCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 20*time.Second)
}
