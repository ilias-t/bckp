package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ilias-t/bckp/internal/config"
	"github.com/ilias-t/bckp/internal/contracts"
	"github.com/ilias-t/bckp/internal/paths"
)

// Short local names for the system tools and locations install uses.
const (
	binLaunchctl   = paths.BinLaunchctl
	binDseditgroup = paths.BinDseditgroup
	daemonLogDir   = paths.DaemonLogDir
)

// daemonBinaryName is the helper's file name next to the backpack binary.
const daemonBinaryName = "backpackd"

// bootstrapAttempts bounds retries of `launchctl bootstrap`, which can
// fail with "Input/output error" right after a bootout.
const bootstrapAttempts = 5

// sudoUID returns the invoking user's uid from SUDO_UID.
func sudoUID(environ []string) (int, error) {
	v, ok := config.LookupEnv(environ, "SUDO_UID")
	if !ok || v == "" {
		return 0, errors.New("SUDO_UID is not set")
	}
	uid, err := strconv.Atoi(v)
	if err != nil || uid < 0 {
		return 0, fmt.Errorf("SUDO_UID %q is not a uid", v)
	}
	return uid, nil
}

// installer prints and (unless dry) performs root file operations.
type installer struct {
	e   *env
	dry bool
}

func (in *installer) say(verb, format string, args ...any) {
	if in.dry {
		verb = "would " + verb
	}
	in.e.printf("  %-14s %s\n", verb, fmt.Sprintf(format, args...))
}

// ensureDir creates dir if missing. With own set, it also makes the
// directory root:wheel 0755 (for directories backpack owns, so no other
// user can swap files inside). An existing directory owned by anyone but
// root is refused rather than adopted: its owner could have left files or
// links in it.
func (in *installer) ensureDir(dir string, own bool) error {
	fi, err := os.Lstat(dir)
	switch {
	case err == nil && !fi.IsDir():
		return fmt.Errorf("%s exists and is not a directory", dir)
	case err == nil && !own:
		return nil
	case err != nil && !errors.Is(err, fs.ErrNotExist):
		return err
	}
	if err != nil {
		in.say("create dir", "%s (root:wheel 0755)", dir)
	} else {
		uid, gid := in.e.ownerOf(fi)
		if uid != 0 {
			return fmt.Errorf("%s exists and is owned by uid %d, not root; remove it (sudo rm -r %q) and run install again", dir, uid, dir)
		}
		if gid == 0 && fi.Mode().Perm() == 0o755 {
			return nil
		}
		in.say("fix dir", "%s (root:wheel 0755)", dir)
	}
	if in.dry {
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if own {
		if err := in.e.chown(dir, 0, 0); err != nil {
			return err
		}
		if err := os.Chmod(dir, 0o755); err != nil {
			return err
		}
		return in.verify(dir, 0o755, true)
	}
	return nil
}

// writeRoot writes data to path as root:wheel with mode, atomically, and
// verifies the result.
func (in *installer) writeRoot(path string, data []byte, mode os.FileMode) error {
	if old, err := os.ReadFile(path); err == nil && bytes.Equal(old, data) {
		if fi, err := os.Lstat(path); err == nil {
			if uid, gid := in.e.ownerOf(fi); uid == 0 && gid == 0 && fi.Mode().Perm() == mode && fi.Mode().IsRegular() {
				in.e.printf("  %-14s %s (root:wheel %04o)\n", "unchanged", path, mode)
				return nil
			}
		}
	}
	in.say("write", "%s (%d bytes, root:wheel %04o)", path, len(data), mode)
	if in.dry {
		return nil
	}
	chownRoot := func(tmp string) error { return in.e.chown(tmp, 0, 0) }
	if err := writeFileAtomic(path, data, mode, chownRoot); err != nil {
		return err
	}
	return in.verify(path, mode, false)
}

// verify checks path's owner (root:wheel), type and permission bits.
func (in *installer) verify(path string, mode os.FileMode, dir bool) error {
	fi, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if dir && !fi.IsDir() || !dir && !fi.Mode().IsRegular() {
		return fmt.Errorf("verify %s: unexpected file type %s", path, fi.Mode().Type())
	}
	if uid, gid := in.e.ownerOf(fi); uid != 0 || gid != 0 {
		return fmt.Errorf("verify %s: owner is %d:%d, want 0:0 (root:wheel)", path, uid, gid)
	}
	if fi.Mode().Perm() != mode {
		return fmt.Errorf("verify %s: mode is %04o, want %04o", path, fi.Mode().Perm(), mode)
	}
	return nil
}

// runCmd prints and (unless dry) runs a command.
func (in *installer) runCmd(ctx context.Context, name string, args ...string) error {
	in.say("run", "%s", contracts.CommandLine(name, args...))
	if in.dry {
		return nil
	}
	_, err := in.e.exec.Run(ctx, name, args...)
	return err
}

// resolveDaemonSource finds the backpackd to install: --daemon-bin, or
// the sibling of the running (symlink-resolved) binary.
func (e *env) resolveDaemonSource(flagPath string) (string, error) {
	src := flagPath
	if src == "" {
		exe, err := e.executable()
		if err != nil {
			return "", fmt.Errorf("locating the backpack binary: %w", err)
		}
		if real, err := filepath.EvalSymlinks(exe); err == nil {
			exe = real
		}
		src = filepath.Join(filepath.Dir(exe), daemonBinaryName)
	}
	abs, err := filepath.Abs(src)
	if err != nil {
		return "", err
	}
	fi, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("daemon binary: %w (build it, or pass --daemon-bin)", err)
	}
	if !fi.Mode().IsRegular() {
		return "", fmt.Errorf("daemon binary %s is not a regular file", abs)
	}
	return abs, nil
}

// parseUIDs parses an allowed_uids file: one decimal uid per line;
// blank lines and # comments are ignored.
func parseUIDs(data []byte) []int {
	var uids []int
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = strings.TrimSpace(line[:i])
		}
		if line == "" {
			continue
		}
		if uid, err := strconv.Atoi(line); err == nil && uid >= 0 {
			uids = append(uids, uid)
		}
	}
	return uids
}

// allowlistFor is the allowlist content: exactly one uid. backpackd
// parses it strictly (decimal uids, one per line, no blanks, spaces or
// comments).
func allowlistFor(uid int) []byte {
	return []byte(strconv.Itoa(uid) + "\n")
}

func cmdInstall(e *env, args []string) int {
	fset := newFlags(e, "install")
	dry := fset.Bool("dry-run", false, "print what would be done without doing it")
	daemonBin := fset.String("daemon-bin", "", "install the backpackd at `PATH` (default: the one next to this binary)")
	// --no-hint (hidden: empty usage) skips the `doctor` hint, for callers
	// that carry on themselves: the start flow and install.sh.
	noHint := fset.Bool("no-hint", false, "")
	pos, code, ok := e.parseArgs(fset, args)
	if !ok {
		return code
	}
	if len(pos) > 0 {
		e.errorf("install takes no arguments")
		return exitUsage
	}

	uid, err := sudoUID(e.environ)
	switch {
	case *dry && err != nil:
		uid = e.getuid()
	case e.getuid() != 0:
		e.errorf("install needs root: sudo %s install", e.selfPath())
		return exitErr
	case err != nil:
		e.errorf("%v; run `sudo %s install` from your own account so backpack knows who to allow", err, e.selfPath())
		return exitErr
	case uid == 0:
		e.errorf("SUDO_UID is 0; run `sudo %s install` from your own (non-root) account", e.selfPath())
		return exitErr
	}
	if e.wiring.DaemonPlist == nil {
		return e.notWired("the daemon plist")
	}
	plist := e.wiring.DaemonPlist()
	if len(plist) == 0 {
		e.errorf("the daemon plist is empty")
		return exitErr
	}
	src, err := e.resolveDaemonSource(*daemonBin)
	if err != nil {
		e.errorf("%v", err)
		return exitErr
	}
	bin, err := os.ReadFile(src)
	if err != nil {
		e.errorf("%v", err)
		return exitErr
	}

	in := &installer{e: e, dry: *dry}
	if *dry {
		e.printf("%s install (dry run; nothing is changed):\n", e.prog)
	} else {
		e.printf("%s install:\n", e.prog)
	}
	binPath, uidsPath, plistPath := e.sys(paths.DaemonBinary), e.sys(paths.AllowedUIDs), e.sys(paths.DaemonPlist)
	if old, err := os.ReadFile(uidsPath); err == nil {
		if prev := parseUIDs(old); len(prev) > 0 && !(len(prev) == 1 && prev[0] == uid) {
			e.printf("  %-14s previous allowlist %v is replaced by uid %d\n", "note", prev, uid)
		}
	}

	steps := []func() error{
		func() error { return in.ensureDir(filepath.Dir(binPath), false) },
		func() error {
			in.e.printf("  %-14s %s\n", "source", src)
			return in.writeRoot(binPath, bin, 0o755)
		},
		func() error { return in.ensureDir(e.sys(paths.SystemSupportDir), true) },
		func() error { return in.writeRoot(uidsPath, allowlistFor(uid), 0o644) },
		func() error { return in.ensureDir(e.sys(daemonLogDir), true) },
		func() error { return in.ensureDir(filepath.Dir(plistPath), false) },
		func() error { return in.writeRoot(plistPath, plist, 0o644) },
	}
	for _, step := range steps {
		if err := step(); err != nil {
			e.errorf("install failed: %v", err)
			return exitErr
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := in.runCmd(ctx, binLaunchctl, "bootout", "system/"+paths.DaemonLabel); err != nil {
		e.printf("  %-14s (not loaded before; fine)\n", "")
	}
	var bootErr error
	for attempt := 1; attempt <= bootstrapAttempts; attempt++ {
		if attempt > 1 {
			e.sleep(500 * time.Millisecond)
		}
		if bootErr = in.runCmd(ctx, binLaunchctl, "bootstrap", "system", plistPath); bootErr == nil {
			break
		}
	}
	if bootErr != nil {
		e.errorf("launchctl bootstrap failed: %v", bootErr)
		return exitErr
	}
	if *dry {
		e.printf("dry run done; run `sudo %s install` to apply.\n", e.selfPath())
		return exitOK
	}
	if *noHint {
		e.printf("installed backpackd for uid %d.\n", uid)
	} else {
		e.printf("installed backpackd for uid %d. Next: `%s doctor`\n", uid, e.prog)
	}
	return exitOK
}

func cmdUninstall(e *env, args []string) int {
	fset := newFlags(e, "uninstall")
	dry := fset.Bool("dry-run", false, "print what would be done without doing it")
	pos, code, ok := e.parseArgs(fset, args)
	if !ok {
		return code
	}
	if len(pos) > 0 {
		e.errorf("uninstall takes no arguments")
		return exitUsage
	}
	if !*dry && e.getuid() != 0 {
		e.errorf("uninstall needs root: sudo %s uninstall", e.selfPath())
		return exitErr
	}
	in := &installer{e: e, dry: *dry}
	if *dry {
		e.printf("%s uninstall (dry run; nothing is changed):\n", e.prog)
	} else {
		e.printf("%s uninstall:\n", e.prog)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	failed := false
	// Re-enable lid-close sleep first, whatever happens next.
	if err := in.runCmd(ctx, paths.BinPmset, "-a", "disablesleep", "0"); err != nil {
		e.errorf("pmset failed: %v", err)
		failed = true
	}
	if err := in.runCmd(ctx, binLaunchctl, "bootout", "system/"+paths.DaemonLabel); err != nil {
		e.printf("  %-14s (not loaded; fine)\n", "")
	}
	for _, p := range []string{e.sys(paths.DaemonPlist), e.sys(paths.DaemonBinary), e.sys(paths.AllowedUIDs)} {
		if _, err := os.Lstat(p); errors.Is(err, fs.ErrNotExist) {
			in.e.printf("  %-14s %s (not present)\n", "skip", p)
			continue
		}
		in.say("remove", "%s", p)
		if !*dry {
			if err := os.Remove(p); err != nil {
				e.errorf("%v", err)
				failed = true
			}
		}
	}
	dir := e.sys(paths.SystemSupportDir)
	if entries, err := os.ReadDir(dir); err == nil && (len(entries) == 0 || *dry && onlyAllowlist(entries)) {
		in.say("remove dir", "%s", dir)
		if !*dry {
			if err := os.Remove(dir); err != nil {
				e.errorf("%v", err)
				failed = true
			}
		}
	}
	if _, err := os.Stat(e.sys(daemonLogDir)); err == nil {
		e.printf("  %-14s %s (daemon logs; remove by hand if you like)\n", "left", e.sys(daemonLogDir))
	}
	if !*dry {
		k := readKernel(ctx, e.exec)
		e.printf("  %-14s %s\n", "kernel", k.summary())
		if k.sleepDisabled() {
			e.errorf("SleepDisabled is still Yes; run: sudo pmset -a disablesleep 0")
			failed = true
		}
	}
	if failed {
		return exitErr
	}
	if *dry {
		e.printf("dry run done; run `sudo %s uninstall` to apply.\n", e.selfPath())
	} else {
		e.printf("uninstalled.\n")
	}
	return exitOK
}

func onlyAllowlist(entries []os.DirEntry) bool {
	return len(entries) == 1 && entries[0].Name() == filepath.Base(paths.AllowedUIDs)
}
