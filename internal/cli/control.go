package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ilias-t/bckp/internal/config"
	"github.com/ilias-t/bckp/internal/contracts"
	"github.com/ilias-t/bckp/internal/humanize"
)

// sleepDisabledWarning is printed whenever the kernel blocks lid-close
// sleep with no session to justify it.
func (e *env) warnSleepDisabledOrphan() {
	fmt.Fprintf(e.stderr, `
!!! WARNING: SleepDisabled=Yes but no backpack session is running.
!!! This Mac will NOT sleep with the lid closed and can overheat in a bag.
!!! Fix now:  sudo pmset -a disablesleep 0
!!! (backpackd also resets it when it restarts: sudo launchctl kickstart -k system/dev.bckp.helper)

`)
}

// statusJSON is `status --json`.
type statusJSON struct {
	Running  bool                `json:"running"`
	Snapshot *contracts.Snapshot `json:"snapshot"`
	Kernel   kernelState         `json:"kernel"`
	Error    string              `json:"error,omitempty"`
}

func cmdStatus(e *env, args []string) int {
	fs := newFlags(e, "status")
	asJSON := fs.Bool("json", false, "print JSON")
	pos, code, ok := e.parseArgs(fs, args)
	if !ok {
		return code
	}
	if len(pos) > 0 {
		e.errorf("status takes no arguments")
		return exitUsage
	}
	ctx, cancel := shortCtx()
	defer cancel()
	snap, err := e.client().Status(ctx)
	kernel := readKernel(ctx, e.exec)
	running := err == nil
	notRunning := errors.Is(err, contracts.ErrEngineNotRunning)

	if *asJSON {
		out := statusJSON{Running: running, Kernel: kernel}
		if running {
			out.Snapshot = &snap
		} else if !notRunning {
			out.Error = err.Error()
		}
		enc := json.NewEncoder(e.stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(out)
	} else if running {
		e.printStatus(snap, kernel)
	} else {
		if notRunning {
			e.printf("%s: not running\n", e.prog)
		} else {
			e.errorf("engine status: %v", err)
		}
		e.printf("  %-9s %s\n", "kernel", kernel.summary())
	}

	switch {
	case !running && kernel.sleepDisabled():
		e.warnSleepDisabledOrphan()
	case running && snap.Lease.Held && kernel.SleepDisabled != nil && !*kernel.SleepDisabled:
		e.errorf("warning: the engine holds a lease but the kernel reports SleepDisabled=No; closing the lid will sleep the Mac")
	}
	switch {
	case running:
		return exitOK
	case notRunning:
		return exitNotRunning
	}
	return exitErr
}

// printStatus renders a snapshot for humans.
func (e *env) printStatus(s contracts.Snapshot, k kernelState) {
	now := e.now()
	row := func(label, format string, args ...any) {
		e.printf("  %-9s %s\n", label, fmt.Sprintf(format, args...))
	}
	e.printf("%s: %s\n", e.prog, readyLine(s, config.Default(), now))

	if len(s.Options) > 0 {
		row("options", "%s", optionsStatus(s.Options))
	}
	ss := s.Session
	sess := fmt.Sprintf("started %s, %s left", humanize.Clock(ss.StartedAt, now), humanize.Duration(time.Duration(ss.RemainingS)*time.Second))
	if ss.Forever {
		sess += " (forever, capped)"
	}
	if ss.Ended {
		sess = "ended (" + string(ss.EndReason) + ")"
	}
	row("session", "%s", sess)

	th := string(s.Thermal.FSM)
	if r := s.Thermal.Reading; r != nil {
		// The kernel's state, when it says more than the FSM's.
		var parts []string
		if !strings.EqualFold(r.State.String(), th) {
			parts = append(parts, r.State.String())
		}
		if r.CPUC != nil {
			parts = append(parts, fmt.Sprintf("CPU %.0f°C", *r.CPUC))
		}
		if r.GPUC != nil {
			parts = append(parts, fmt.Sprintf("GPU %.0f°C", *r.GPUC))
		}
		if len(parts) > 0 {
			th += " (" + strings.Join(parts, ", ") + ")"
		}
	}
	if !s.Thermal.HotSince.IsZero() {
		th += ", hot since " + humanize.Clock(s.Thermal.HotSince, now)
	}
	row("thermal", "%s", th)

	if p := s.SleepPending; p != nil {
		abort := fmt.Sprintf("abortable with `%s abort`", e.prog)
		if !p.Abortable {
			abort = "cannot be aborted"
		}
		row("SLEEP", "in %ds (%s: %s; %s)", p.RemainingS, p.Trigger, p.Reason, abort)
	}

	if p := s.Power; p != nil {
		src := "on battery"
		if p.OnAC {
			src = "on AC"
		}
		if p.Charging {
			src += ", charging"
		}
		lid := "lid open"
		if p.LidClosed {
			lid = "lid closed"
		}
		row("power", "%d%% %s, %s", p.Percent, src, lid)
	}

	n := s.Network
	net := networkModeText(n.Mode)
	if n.Mode != contracts.NetworkOff {
		if n.Online {
			net += ", online"
			if n.Link != "" && n.Link != contracts.LinkNone {
				net += " via " + n.Link.Label()
			}
		} else if !n.OfflineSince.IsZero() {
			net += ", offline since " + humanize.Clock(n.OfflineSince, now)
		} else {
			net += ", offline"
		}
		if n.Recoveries > 0 {
			net += fmt.Sprintf(", %d recoveries", n.Recoveries)
		}
		if n.WiFiNudges > 0 {
			last := n.LastWiFiNudgeResult
			if last == "" {
				last = "pending"
			}
			net += fmt.Sprintf(", %d Wi-Fi nudges (last %s: %s)", n.WiFiNudges, humanize.Clock(n.LastWiFiNudge, now), last)
		}
	}
	row("network", "%s", net)

	if s.Lease.Held {
		row("lease", "held, expires %s", s.Lease.Expires.In(now.Location()).Format("15:04:05"))
	} else {
		row("lease", "NOT held")
	}
	if s.OutboxPending > 0 {
		row("outbox", "%d notifications waiting", s.OutboxPending)
	}
	if len(s.Errors) > 0 {
		keys := make([]string, 0, len(s.Errors))
		for k := range s.Errors {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, key := range keys {
			row("error", "%s: %s", key, s.Errors[key])
		}
	}
	row("kernel", "%s", k.summary())
}

// networkModeText describes the engine's network mode for `status`.
func networkModeText(m contracts.NetworkMode) string {
	switch m {
	case contracts.NetworkPreferHotspot:
		return "hotspot preferred"
	case contracts.NetworkHotspotAuto:
		return "hotspot auto (waiting for the phone)"
	case contracts.NetworkWatchdog:
		return "watchdog"
	}
	return string(m)
}

// cmdAbort cancels a pending abortable (serious-heat) sleep. Unlike
// extend it adds no time and leaves the max_duration cap alone.
func cmdAbort(e *env, args []string) int {
	fs := newFlags(e, "abort")
	pos, code, ok := e.parseArgs(fs, args)
	if !ok {
		return code
	}
	if len(pos) > 0 {
		e.errorf("abort takes no arguments")
		return exitUsage
	}
	ctx, cancel := opCtx()
	defer cancel()
	snap, err := e.client().AbortSleep(ctx)
	switch {
	case errors.Is(err, contracts.ErrEngineNotRunning):
		e.printf("%s: not running\n", e.prog)
		return exitNotRunning
	case errors.Is(err, contracts.ErrNoSleepPending):
		e.printf("%s: no sleep pending\n", e.prog)
		return exitOK
	case errors.Is(err, contracts.ErrNotAbortable):
		e.errorf("a critical-heat sleep is pending and cannot be aborted; the Mac will sleep in a few seconds")
		return exitErr
	case err != nil:
		e.errorf("abort: %v", err)
		return exitErr
	}
	e.printf("%s: pending sleep cancelled; still watching the heat. %s\n", e.prog, readyLine(snap, config.Default(), e.now()))
	return exitOK
}

func cmdStop(e *env, args []string) int {
	fs := newFlags(e, "stop")
	doSleep := fs.Bool("sleep", false, "sleep the Mac after stopping")
	noSleep := fs.Bool("no-sleep", false, "don't sleep; cancels a pending (abortable) thermal sleep")
	pos, code, ok := e.parseArgs(fs, args)
	if !ok {
		return code
	}
	if len(pos) > 0 {
		e.errorf("stop takes no arguments")
		return exitUsage
	}
	if *doSleep && *noSleep {
		e.errorf("--sleep and --no-sleep conflict")
		return exitUsage
	}
	var sleep *bool
	switch {
	case *doSleep:
		sleep = doSleep
	case *noSleep:
		f := false
		sleep = &f
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err := e.client().Stop(ctx, sleep)
	switch {
	case errors.Is(err, contracts.ErrEngineNotRunning):
		e.restoreStaleJournal()
		e.printf("%s: not running\n", e.prog)
		if readKernel(ctx, e.exec).sleepDisabled() {
			e.warnSleepDisabledOrphan()
		}
		return exitNotRunning
	case errors.Is(err, contracts.ErrNotAbortable):
		e.errorf("a critical-heat sleep is pending and cannot be aborted; the Mac will sleep in a few seconds")
		return exitErr
	case err != nil:
		e.errorf("stop: %v", err)
		return exitErr
	}
	if *doSleep {
		e.printf("%s: stopped; sleeping now\n", e.prog)
	} else {
		e.printf("%s: stopped\n", e.prog)
	}
	return exitOK
}

func cmdExtend(e *env, args []string) int {
	fs := newFlags(e, "extend")
	pos, code, ok := e.parseArgs(fs, args)
	if !ok {
		return code
	}
	if len(pos) != 1 {
		e.errorf("usage: %s extend N|DUR|forever", e.prog)
		return exitUsage
	}
	d, forever, err := parseSessionDuration(pos[0])
	if err != nil {
		e.errorf("%v", err)
		return exitUsage
	}
	minutes := int((d + time.Minute - 1) / time.Minute)
	ctx, cancel := opCtx()
	defer cancel()
	snap, err := e.client().Extend(ctx, minutes, forever)
	switch {
	case errors.Is(err, contracts.ErrEngineNotRunning):
		e.printf("%s: not running\n", e.prog)
		return exitNotRunning
	case errors.Is(err, contracts.ErrNotAbortable):
		e.errorf("a critical-heat sleep is pending and cannot be postponed")
		return exitErr
	case err != nil:
		e.errorf("extend: %v", err)
		return exitErr
	}
	e.printf("%s: extended; %s\n", e.prog, readyLine(snap, config.Default(), e.now()))
	return exitOK
}

func cmdAttach(e *env, args []string) int {
	fs := newFlags(e, "attach")
	pos, code, ok := e.parseArgs(fs, args)
	if !ok {
		return code
	}
	if len(pos) > 0 {
		e.errorf("attach takes no arguments")
		return exitUsage
	}
	ctx, cancel := shortCtx()
	defer cancel()
	client := e.client()
	if _, err := client.Status(ctx); err != nil {
		if errors.Is(err, contracts.ErrEngineNotRunning) {
			e.printf("%s: not running\n", e.prog)
			return exitNotRunning
		}
		e.errorf("status: %v", err)
		return exitErr
	}
	if !e.isTTY() {
		e.errorf("attach needs a terminal; use `%s status`", e.prog)
		return exitUsage
	}
	if e.wiring.RunTUI == nil {
		return e.notWired("the TUI")
	}
	return attachTUI(e, client)
}

func cmdNotify(e *env, args []string) int {
	fs := newFlags(e, "notify")
	cfgPath := fs.String("config", "", "read the config from `PATH`")
	pos, code, ok := e.parseArgs(fs, args)
	if !ok {
		return code
	}
	if len(pos) != 1 || pos[0] != "test" {
		e.errorf("usage: %s notify test", e.prog)
		return exitUsage
	}
	cfg, _, err := e.loadConfig(*cfgPath)
	if err != nil {
		e.errorf("%v", err)
		return exitErr
	}
	results, err := e.sendTestNotification(cfg)
	if err != nil && results == "" {
		e.errorf("%v", err)
		return exitErr
	}
	if err != nil {
		e.errorf("test notification: %s%s", results, topicHint(cfg))
		return exitErr
	}
	e.printf("%s: test sent via %s%s\n", e.prog, results, topicHint(cfg))
	return exitOK
}

func topicHint(cfg config.Config) string {
	if cfg.Notify.NtfyTopic == "" {
		return " (no ntfy topic set; only local notifiers)"
	}
	return fmt.Sprintf(" (ntfy: %s/%s)", strings.TrimRight(cfg.Notify.NtfyServer, "/"), maskTopic(cfg.Notify.NtfyTopic))
}

// maskTopic hides most of an ntfy topic: anyone who knows it can read and
// post to it.
func maskTopic(t string) string {
	if len(t) <= 4 {
		return "****"
	}
	return t[:2] + strings.Repeat("*", len(t)-4) + t[len(t)-2:]
}

// sendTestNotification sends one test notification through every
// configured backend and waits for each answer. results reads
// "ntfy ✓ · macOS ✓ · log ✓", with "✗ (error)" for a failure; err is
// non-nil when any backend failed. The log backend writes to cli.jsonl,
// never to the terminal.
func (e *env) sendTestNotification(cfg config.Config) (results string, err error) {
	if e.wiring.NewNotifiers == nil {
		return "", errors.New("notifiers are not wired in this build")
	}
	log, closeLog := e.cliLog(cfg)
	defer closeLog()
	host, _ := os.Hostname()
	n := contracts.Notification{
		Priority: contracts.PriorityDefault,
		Title:    "backpack test",
		Body:     fmt.Sprintf("Test notification from %s at %s.", host, e.now().Format("15:04")),
		Tags:     []string{"test_tube"},
		// The push policy (notify.push) lives in the engine's send path;
		// this goes straight to every backend, so it is always pushed.
		Class: contracts.ClassAttention,
	}
	backends := e.wiring.NewNotifiers(cfg, log)
	if len(backends) == 0 {
		return "", errors.New("no notification backends configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	errs := make([]error, len(backends))
	var wg sync.WaitGroup
	for i, b := range backends {
		wg.Go(func() { errs[i] = b.Send(ctx, n) })
	}
	wg.Wait()
	parts := make([]string, len(backends))
	var failed []error
	for i, b := range backends {
		if errs[i] != nil {
			parts[i] = fmt.Sprintf("%s ✗ (%s)", b.Name, redactTopic(errs[i].Error(), cfg.Notify.NtfyTopic))
			failed = append(failed, fmt.Errorf("%s: %s", b.Name, redactTopic(errs[i].Error(), cfg.Notify.NtfyTopic)))
			continue
		}
		parts[i] = b.Name + " ✓"
	}
	if len(failed) > 0 {
		err = fmt.Errorf("test notification failed: %w", errors.Join(failed...))
	}
	return strings.Join(parts, " · "), err
}

// redactTopic replaces the ntfy topic (a secret) in s with "<redacted>",
// as the event logs do.
func redactTopic(s, topic string) string {
	if topic == "" {
		return s
	}
	return strings.ReplaceAll(s, topic, "<redacted>")
}

func cmdSimulate(e *env, args []string) int {
	fs := newFlags(e, "simulate")
	cfgPath := fs.String("config", "", "read the config from `PATH`")
	fs.notes = e.wiring.SimulateGrammar
	pos, code, ok := e.parseArgs(fs, args)
	if !ok {
		return code
	}
	script := strings.TrimSpace(strings.Join(pos, " "))
	if script == "" {
		fmt.Fprintf(e.stderr, "%s\n\n%s", fs.usageLine(), e.wiring.SimulateGrammar)
		return exitUsage
	}
	if e.wiring.Simulate == nil {
		return e.notWired("simulate")
	}
	cfg, _, err := e.loadConfig(*cfgPath)
	if err != nil {
		e.errorf("%v", err)
		return exitErr
	}
	ctx, cancel := e.signalContext(context.Background())
	defer cancel()
	err = e.wiring.Simulate(ctx, script, cfg, e.stdout)
	switch {
	case errors.Is(err, contracts.ErrBadScript):
		e.errorf("simulate: %v", err)
		fmt.Fprintf(e.stderr, "\n%s", e.wiring.SimulateGrammar)
		return exitUsage
	case err != nil:
		e.errorf("simulate: %v", err)
		return exitErr
	}
	return exitOK
}

func cmdConfig(e *env, args []string) int {
	fset := newFlags(e, "config")
	cfgPath := fset.String("config", "", "read the config from `PATH`")
	force := fset.Bool("force", false, "init: overwrite an existing file")
	pos, code, ok := e.parseArgs(fset, args)
	if !ok {
		return code
	}
	if len(pos) != 1 {
		e.errorf("usage: %s config init|path|show", e.prog)
		return exitUsage
	}
	path, _ := config.ResolvePath(*cfgPath, e.environ, e.user)
	switch pos[0] {
	case "path":
		e.printf("%s\n", path)
		if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
			e.errorf("(not created yet; run `%s config init`)", e.prog)
		}
		return exitOK
	case "show":
		cfg, _, err := e.loadConfig(*cfgPath)
		if err != nil {
			e.errorf("%v", err)
			return exitErr
		}
		e.printf("# effective config: %s + defaults + BACKPACK_* overrides\n", path)
		if cfg.Notify.NtfyTopic != "" {
			// The topic is a secret, and this output gets pasted into
			// bug reports.
			e.printf("# notify.ntfy_topic is masked\n")
			cfg.Notify.NtfyTopic = maskTopic(cfg.Notify.NtfyTopic)
		}
		if err := cfg.WriteTOML(e.stdout); err != nil {
			e.errorf("%v", err)
			return exitErr
		}
		return exitOK
	case "init":
		if _, err := os.Stat(path); err == nil && !*force {
			e.errorf("%s already exists (use --force to overwrite)", path)
			return exitErr
		}
		if err := e.writeConfig(path, []byte(configTemplate)); err != nil {
			e.errorf("%v", err)
			return exitErr
		}
		e.printf("wrote %s\n", path)
		return exitOK
	}
	e.errorf("unknown config command %q (want init, path or show)", pos[0])
	return exitUsage
}

// writeFileAtomic writes data to a temp file in path's directory, syncs it,
// sets perm, runs before on the temp file (install uses it to chown), and
// renames it into place.
func writeFileAtomic(path string, data []byte, perm os.FileMode, before func(tmp string) error) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	_, err = tmp.Write(data)
	if err == nil {
		err = tmp.Sync()
	}
	if err == nil {
		err = tmp.Chmod(perm)
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil && before != nil {
		err = before(name)
	}
	if err != nil {
		return err
	}
	return os.Rename(name, path)
}
