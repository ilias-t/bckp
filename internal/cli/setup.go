package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/ilias-t/bckp/internal/config"
	"github.com/ilias-t/bckp/internal/contracts"
	"github.com/ilias-t/bckp/internal/eventlog"
	"github.com/ilias-t/bckp/internal/paths"
)

// Setup prompts (docs/DESIGN.md "Prompts"): the daemon install offer,
// first-run config setup, and the empty ntfy topic offer. Each prompts only
// in a terminal and without --yes; otherwise it prints what to do and
// takes the non-interactive default (refuse without the daemon, run on
// defaults without a config file, warn about an empty topic).

// daemonWait bounds how long the start waits for a freshly installed
// daemon to answer.
const (
	daemonWaitTries    = 10
	daemonWaitInterval = 300 * time.Millisecond
)

// runInteractive runs a command with the terminal attached (stdin,
// stdout, stderr inherited), so sudo can ask for the password.
func runInteractive(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

// daemonStatus asks backpackd for its status.
func (e *env) daemonStatus() error {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err := e.wiring.NewGuard().Status(ctx)
	return err
}

// ensureDaemon refuses to start a session without a reachable backpackd:
// without it nothing keeps a closed lid awake. In a terminal (and without
// --yes) it offers to run `sudo <self> install` right away.
//
// With no daemon client wired (a test build), the check is skipped; the
// engine's lease request would fail anyway.
func (e *env) ensureDaemon(interactive bool) int {
	if e.wiring.NewGuard == nil {
		return exitOK
	}
	err := e.daemonStatus()
	if err == nil {
		return exitOK
	}
	self := e.selfPath()
	why := "is not installed or not answering"
	if errors.Is(err, contracts.ErrNotAllowed) {
		why = fmt.Sprintf("does not allow this user (uid %d)", e.getuid())
	}
	if !interactive {
		e.errorf("backpackd (the root helper that keeps a closed lid awake) %s: %v", why, err)
		e.errorf("install it once with: sudo %s install", self)
		return exitErr
	}
	p := e.prompter()
	fmt.Fprintf(e.stdout, "backpackd, the small root helper that keeps a closed lid awake, %s.\n", why)
	ok, perr := p.yesNo(fmt.Sprintf("Install it now with `sudo %s install`?", self), true)
	if perr != nil {
		e.errorf("%v", perr)
		return exitErr
	}
	if !ok {
		e.errorf("not starting: without backpackd a closed lid still sleeps. Install later with: sudo %s install", self)
		return exitErr
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if err := e.interactive(ctx, paths.BinSudo, self, "install", "--no-hint"); err != nil {
		e.errorf("sudo %s install failed: %v; not starting", self, err)
		return exitErr
	}
	for try := 0; try < daemonWaitTries; try++ {
		if err = e.daemonStatus(); err == nil {
			fmt.Fprintf(e.stdout, "backpackd is answering ✓\n")
			return exitOK
		}
		e.sleep(daemonWaitInterval)
	}
	e.errorf("backpackd still does not answer after the install (%v); not starting. Try `%s doctor`.", err, e.prog)
	return exitErr
}

// selfPath is the running binary, for the sudo command line.
func (e *env) selfPath() string {
	exe, err := e.executable()
	if err != nil || exe == "" {
		return e.prog
	}
	return exe
}

// firstRun runs the short setup when there is no config file yet (only at
// the default location; an explicit --config must exist). It reports
// whether it asked about the ntfy topic, so the empty-topic offer does not
// ask twice.
func (e *env) firstRun(path string) (askedTopic bool, code int) {
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		return false, exitOK
	}
	p := e.prompter()
	fmt.Fprintf(e.stdout, "No backpack config yet. Quick setup (Enter skips it):\n")
	fmt.Fprintf(e.stdout, "With the lid closed you won't see macOS banners; heat alerts reach you on your phone\n")
	fmt.Fprintf(e.stdout, "through the ntfy app (no account needed).\n")
	topic, err := e.askTopic(p, config.Default().Notify.NtfyServer)
	if err != nil {
		e.errorf("%v", err)
		return true, exitErr
	}
	data := []byte(configTemplate)
	if topic != "" {
		data = config.SetString(data, "notify", "ntfy_topic", topic)
	}
	if err := e.writeConfig(path, data); err != nil {
		e.errorf("writing %s: %v", path, err)
		return true, exitErr
	}
	fmt.Fprintf(e.stdout, "wrote %s\n", path)
	return true, exitOK
}

// ensureTopic handles an empty ntfy topic. With macOS notifications on
// (the default) it says nothing: first-run setup offered a topic, and
// `doctor` keeps the tip. With them off nothing would reach the user, so it
// warns and, when interactive, offers to set a topic in the config file.
// It returns the (possibly reloaded) config.
func (e *env) ensureTopic(cfg config.Config, path string, interactive bool) (config.Config, int) {
	if cfg.Notify.NtfyTopic != "" || cfg.Notify.MacOS {
		return cfg, exitOK
	}
	e.errorf("warning: notify.macos is off and notify.ntfy_topic is not set, so alerts only go to the log")
	if !interactive {
		return cfg, exitOK
	}
	topic, err := e.askTopic(e.prompter(), cfg.Notify.NtfyServer)
	if err != nil {
		e.errorf("%v", err)
		return cfg, exitErr
	}
	if topic == "" {
		return cfg, exitOK
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		data, err = []byte(configTemplate), nil
	}
	if err == nil {
		err = e.writeConfig(path, config.SetString(data, "notify", "ntfy_topic", topic))
	}
	if err != nil {
		e.errorf("saving the topic to %s: %v", path, err)
		return cfg, exitErr
	}
	fmt.Fprintf(e.stdout, "saved to %s\n", path)
	cfg.Notify.NtfyTopic = topic
	return cfg, exitOK
}

// askTopic asks for an ntfy topic: a pasted one, "g" for a generated one,
// or Enter to skip ("").
func (e *env) askTopic(p *prompter, server string) (string, error) {
	topic, err := p.choose("ntfy topic for phone alerts: paste one, [g]enerate a random one, or Enter to skip:", "",
		func(s string) (string, error) {
			if strings.EqualFold(s, "g") {
				return e.generateTopic()
			}
			if !config.ValidTopic(s) {
				return "", fmt.Errorf("a topic is 1-64 letters, digits, - or _")
			}
			return s, nil
		})
	if err != nil || topic == "" {
		return topic, err
	}
	fmt.Fprintf(e.stdout, "ntfy topic: %s\nSubscribe to it in the ntfy app (%s/%s), then check with `%s notify test`.\n",
		topic, strings.TrimRight(server, "/"), topic, e.prog)
	return topic, nil
}

const topicAlphabet = "abcdefghijklmnopqrstuvwxyz0123456789"

// topicLen is the length of a generated topic.
const topicLen = 24

// generateTopic returns topicLen random letters and digits, with nothing
// identifying (no user name or app name): anyone who knows a topic can read
// it, and ntfy.sh topics are public.
func (e *env) generateTopic() (string, error) {
	b := make([]byte, topicLen)
	if _, err := io.ReadFull(e.random, b); err != nil {
		return "", fmt.Errorf("generating a topic: %w", err)
	}
	for i := range b {
		b[i] = topicAlphabet[int(b[i])%len(topicAlphabet)]
	}
	return string(b), nil
}

// configTemplate is what `config init` and first-run setup write: the
// keys that matter, commented out, so that every other key (and a later
// release's defaults) keeps coming from config.Default. The section headers
// are live so an uncommented key (or a chosen topic) lands in its section.
const configTemplate = `# backpack config. Uncomment a key to change it; everything else keeps
# its default. ` + "`bckp config show`" + ` prints every key with its value.

[session]
# max_duration = "12h"       # cap on every session, even "forever"

[power]
# battery_floor = 25         # end the session at this battery %

[network]
# prefer_hotspot = "auto"    # "auto" | "on" | "off"
# watchdog = true            # watch the network, alert on outages
# service = "iPhone USB"     # the hotspot's network service name

[notify]
# ntfy_topic = ""            # your ntfy topic, for phone alerts
# macos = true               # macOS notifications
# push = "attention"         # "all" pushes routine events too
`

// writeConfig validates data and writes it (0600: the ntfy topic is
// effectively a password).
func (e *env) writeConfig(path string, data []byte) error {
	if _, err := config.Parse(data); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return writeFileAtomic(path, data, 0o600, nil)
}

// cliLog opens the CLI's event log, cli.jsonl next to the engine's
// (src=cli). It is a separate file because each eventlog.Writer tracks its
// file's size and rotates it in-process: a second process appending to
// engine.jsonl would have the file renamed away under it and break the
// size cap. `backpack logs` merges every *.jsonl in the directory. On
// error it returns a discarding logger: a missing log never fails a
// command.
func (e *env) cliLog(cfg config.Config) (*slog.Logger, func()) {
	o := eventlog.Options{Src: eventlog.SrcCLI, Redact: []string{cfg.Notify.NtfyTopic}}.WithConfig(cfg.Log.MaxMB, cfg.Log.MaxAge)
	log, closer, err := eventlog.Open(e.user.LogDir, eventlog.CLIName, o)
	if err != nil {
		return slog.New(slog.DiscardHandler), func() {}
	}
	return log, func() { closer.Close() }
}
