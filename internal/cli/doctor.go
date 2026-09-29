package cli

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ilias-t/bckp/internal/config"
	"github.com/ilias-t/bckp/internal/contracts"
	"github.com/ilias-t/bckp/internal/paths"
	"github.com/ilias-t/bckp/internal/version"
)

type checkLevel int

const (
	checkOK checkLevel = iota
	// checkInfo ("i") is an optional extra that is not set up; it is not
	// counted as a warning.
	checkInfo
	// checkWarn is advisory ("!"); it does not fail doctor.
	checkWarn
	// checkFail ("✗") makes doctor exit non-zero.
	checkFail
)

type check struct {
	level  checkLevel
	name   string
	detail string
	fix    string
}

func (c check) mark() string {
	switch c.level {
	case checkOK:
		return "✓"
	case checkInfo:
		return "i"
	case checkWarn:
		return "!"
	}
	return "✗"
}

// invokingUID is the real user's uid (SUDO_UID under sudo).
func (e *env) invokingUID() int {
	if uid, err := sudoUID(e.environ); err == nil && e.getuid() == 0 {
		return uid
	}
	return e.getuid()
}

// checkRootFile verifies an installed file: present, regular, root:wheel,
// with mode.
func (e *env) checkRootFile(name, path string, mode os.FileMode) check {
	fix := fmt.Sprintf("sudo %s install", e.selfPath())
	fi, err := os.Lstat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return check{checkFail, name, path + " is missing", fix}
	case err != nil:
		return check{checkFail, name, err.Error(), fix}
	case !fi.Mode().IsRegular():
		return check{checkFail, name, path + " is not a regular file", fix}
	}
	uid, gid := e.ownerOf(fi)
	if uid != 0 || gid != 0 {
		return check{checkFail, name, fmt.Sprintf("%s is owned by %d:%d, want root:wheel", path, uid, gid), fix}
	}
	if fi.Mode().Perm() != mode {
		return check{checkFail, name, fmt.Sprintf("%s has mode %04o, want %04o", path, fi.Mode().Perm(), mode), fix}
	}
	return check{checkOK, name, fmt.Sprintf("%s (root:wheel %04o)", path, mode), ""}
}

// daemonChecks are the install and daemon-reachability checks. guard is
// the daemon's status when it answered.
func (e *env) daemonChecks(ctx context.Context) (checks []check, guard *contracts.GuardStatus) {
	checks = append(checks,
		e.checkRootFile("daemon binary", e.sys(paths.DaemonBinary), 0o755),
		e.checkRootFile("daemon plist", e.sys(paths.DaemonPlist), 0o644),
	)
	allow := e.checkRootFile("allowlist", e.sys(paths.AllowedUIDs), 0o644)
	if allow.level == checkOK {
		data, err := os.ReadFile(e.sys(paths.AllowedUIDs))
		uid := e.invokingUID()
		if err != nil {
			allow = check{checkFail, "allowlist", err.Error(), ""}
		} else if ferr := strictAllowlistErr(data); ferr != nil {
			allow = check{checkFail, "allowlist", fmt.Sprintf("%s: %v (backpackd will reject it)", e.sys(paths.AllowedUIDs), ferr),
				fmt.Sprintf("sudo %s install", e.selfPath())}
		} else if !slices.Contains(parseUIDs(data), uid) {
			allow = check{checkFail, "allowlist", fmt.Sprintf("uid %d is not in %s", uid, e.sys(paths.AllowedUIDs)),
				fmt.Sprintf("sudo %s install (as this user)", e.selfPath())}
		}
	}
	checks = append(checks, allow)

	if e.getuid() == 0 {
		// root is deliberately not in the allowlist.
		return append(checks, check{checkWarn, "daemon", "running as root; socket check skipped (run doctor as your own user)", ""}), nil
	}
	if e.wiring.NewGuard == nil {
		return append(checks, check{checkFail, "daemon", "daemon client not wired in this build", ""}), nil
	}
	sctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	st, err := e.wiring.NewGuard().Status(sctx)
	switch {
	case errors.Is(err, contracts.ErrNotAllowed):
		checks = append(checks, check{checkFail, "daemon", "backpackd rejected this user", fmt.Sprintf("sudo %s install (as this user)", e.selfPath())})
	case err != nil:
		checks = append(checks, check{checkFail, "daemon", fmt.Sprintf("no answer on %s: %v", paths.DaemonSocket, err),
			fmt.Sprintf("sudo launchctl kickstart -k system/%s, or sudo %s install", paths.DaemonLabel, e.selfPath())})
	default:
		guard = &st
		lease := "no lease"
		if st.Lease != nil {
			lease = "lease held by uid " + strconv.Itoa(st.Lease.UID)
		}
		lvl := checkOK
		if strings.HasPrefix(st.Backstop, contracts.BackstopTrippedPrefix) {
			lvl = checkWarn
		}
		checks = append(checks, check{lvl, "daemon", fmt.Sprintf("responding (%s, backstop %s)", lease, st.Backstop), ""})
	}
	return checks, guard
}

// strictAllowlistErr applies backpackd's allowlist format: decimal uids,
// one per line, each ending in \n (the last optionally), no blank lines,
// spaces, comments or CR, at most 4 KB.
func strictAllowlistErr(data []byte) error {
	if len(data) > 4096 {
		return errors.New("larger than 4 KB")
	}
	if len(data) == 0 {
		return errors.New("empty")
	}
	for i, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		if line == "" {
			return fmt.Errorf("line %d is blank", i+1)
		}
		for _, r := range line {
			if r < '0' || r > '9' {
				return fmt.Errorf("line %d is not a bare decimal uid", i+1)
			}
		}
	}
	return nil
}

func (e *env) checkAdmin(ctx context.Context) check {
	if e.userName == "" {
		return check{checkFail, "admin group", "cannot tell who you are ($USER is empty)", ""}
	}
	res, err := e.exec.Run(ctx, binDseditgroup, "-o", "checkmember", "-m", e.userName, "admin")
	out := strings.TrimSpace(string(res.Stdout))
	if err == nil && strings.HasPrefix(out, "yes") {
		return check{checkOK, "admin group", e.userName + " is an admin (networksetup works without sudo)", ""}
	}
	if out == "" && err != nil {
		out = err.Error()
	}
	return check{checkFail, "admin group", e.userName + " is not in the admin group: " + out,
		"network recovery needs an admin user; add yourself in System Settings > Users & Groups"}
}

// checkHotspotWiFi reminds, when prefer_hotspot = "on" configures the
// Wi-Fi nudge, that macOS must be allowed to join the hotspot on its own.
// The setting (System Settings > Wi-Fi > Ask to join hotspots) has no
// reliable read-only command-line reading: it lives in
// /Library/Preferences/SystemConfiguration/com.apple.airport.preferences.plist,
// which is mode 0644 yet refuses reads (EPERM) outside Apple's own
// processes, and sits next to the known-network (SSID) list backpack never
// touches. So this is informational, never checked.
func checkHotspotWiFi(cfg config.Config) *check {
	if cfg.Network.PreferHotspot != config.HotspotOn || cfg.Network.WiFiNudgeAfter <= 0 {
		return nil
	}
	return &check{checkWarn, "wifi hotspot",
		"for Wi-Fi hotspot fallback set System Settings → Wi-Fi → Ask to join hotspots → Automatic (macOS does not expose it, so not checked)", ""}
}

// checkMacmon looks for the optional macmon. Without it everything works
// on the kernel's thermal state alone; macmon only adds CPU/GPU °C.
func (e *env) checkMacmon() check {
	for _, c := range paths.MacmonCandidates {
		if fi, err := os.Stat(e.sys(c)); err == nil && fi.Mode().IsRegular() {
			return check{checkOK, "macmon", c + " (°C readings)", ""}
		}
	}
	return check{checkInfo, "macmon", "optional, not installed (thermal state only)", "brew install macmon for °C readings"}
}

func cmdDoctor(e *env, args []string) int {
	fset := newFlags(e, "doctor")
	fix := fset.Bool("fix", false, "restore a stale network journal")
	cfgPath := fset.String("config", "", "read the config from `PATH`")
	pos, code, ok := e.parseArgs(fset, args)
	if !ok {
		return code
	}
	if len(pos) > 0 {
		e.errorf("doctor takes no arguments")
		return exitUsage
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	checks, guard := e.daemonChecks(ctx)
	checks = append(checks, e.checkAdmin(ctx), e.checkMacmon())

	cfg, path, err := e.loadConfig(*cfgPath)
	if err != nil {
		checks = append(checks, check{checkFail, "config", err.Error(), "fix the file, or `" + e.prog + " config init --force`"})
	} else {
		if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
			checks = append(checks, check{checkWarn, "config", path + " not created (defaults in use)",
				"`" + e.prog + "` in a terminal runs a short setup, or `" + e.prog + " config init`"})
		} else {
			checks = append(checks, check{checkOK, "config", path + " is valid", ""})
		}
		checks = append(checks, alertsCheck(cfg, path))
		if c := checkHotspotWiFi(cfg); c != nil {
			checks = append(checks, *c)
		}
	}

	snap, engErr := e.client().Status(ctx)
	running := engErr == nil
	switch {
	case running:
		checks = append(checks, check{checkOK, "session", fmt.Sprintf("running (pid %d)", snap.PID), ""})
	case errors.Is(engErr, contracts.ErrEngineNotRunning):
		checks = append(checks, check{checkOK, "session", "none running", ""})
	default:
		checks = append(checks, check{checkWarn, "session", "engine status failed: " + engErr.Error(), ""})
	}

	if !running {
		checks = append(checks, e.checkJournal(ctx, *fix))
	}

	kernel := readKernel(ctx, e.exec)
	leaseHeld := running && snap.Lease.Held || guard != nil && guard.Lease != nil
	switch {
	case kernel.SleepDisabled == nil:
		checks = append(checks, check{checkWarn, "kernel", "cannot read SleepDisabled: " + kernel.Error, ""})
	case *kernel.SleepDisabled && !leaseHeld:
		checks = append(checks, check{checkFail, "kernel", "SleepDisabled=Yes with no lease: lid-close sleep is blocked",
			fmt.Sprintf("sudo pmset -a disablesleep 0 (or restart the daemon, which resets it: sudo launchctl kickstart -k system/%s)", paths.DaemonLabel)})
	default:
		checks = append(checks, check{checkOK, "kernel", kernel.summary(), ""})
	}

	e.printf("%s doctor (%s)\n", e.prog, version.String())
	printChecks(e, checks)
	return summarize(e, checks)
}

// alertsCheck reports where alerts go: macOS notifications and, with a
// topic, the phone through ntfy.
func alertsCheck(cfg config.Config, path string) check {
	n := cfg.Notify
	switch {
	case n.NtfyTopic != "" && n.MacOS:
		return check{checkOK, "alerts", "macOS notifications and ntfy (" + maskTopic(n.NtfyTopic) + ")", ""}
	case n.NtfyTopic != "":
		return check{checkOK, "alerts", "ntfy (" + maskTopic(n.NtfyTopic) + ")", ""}
	case n.MacOS:
		return check{checkInfo, "alerts", "macOS notifications only", "add an ntfy topic for your phone: notify.ntfy_topic in " + path}
	}
	return check{checkWarn, "alerts", "notify.macos is off and notify.ntfy_topic is not set: alerts only go to the log",
		"set notify.macos = true, or notify.ntfy_topic for your phone, in " + path}
}

func printChecks(e *env, checks []check) {
	for _, c := range checks {
		e.printf("  %s %-13s %s\n", c.mark(), c.name, c.detail)
		switch {
		case c.fix == "" || c.level == checkOK:
		case c.level == checkInfo:
			e.printf("    %-13s tip: %s\n", "", c.fix)
		default:
			e.printf("    %-13s fix: %s\n", "", c.fix)
		}
	}
}

func summarize(e *env, checks []check) int {
	var fails, warns int
	for _, c := range checks {
		switch c.level {
		case checkFail:
			fails++
		case checkWarn:
			warns++
		}
	}
	switch {
	case fails > 0:
		e.printf("%d problem(s), %d warning(s).\n", fails, warns)
		return exitErr
	case warns > 0:
		e.printf("all good, %d warning(s).\n", warns)
	default:
		e.printf("all good.\n")
	}
	return exitOK
}

// checkJournal flags (and with fix, restores) a stale restore journal.
func (e *env) checkJournal(ctx context.Context, fix bool) check {
	if _, err := os.Stat(e.user.RestoreJournal); errors.Is(err, fs.ErrNotExist) {
		return check{checkOK, "net journal", "no stale restore journal", ""}
	}
	if !fix {
		return check{checkFail, "net journal", "stale " + filepath.Base(e.user.RestoreJournal) + ": network settings from a crashed session were not restored",
			e.prog + " doctor --fix"}
	}
	if e.wiring.RestoreJournal == nil {
		return check{checkFail, "net journal", "stale journal, and restore is not wired in this build", ""}
	}
	restored, err := e.wiring.RestoreJournal(ctx)
	if err != nil {
		return check{checkFail, "net journal", "restore failed: " + err.Error(), ""}
	}
	if !restored {
		return check{checkOK, "net journal", "nothing to restore", ""}
	}
	return check{checkOK, "net journal", "restored network settings from the stale journal", ""}
}
