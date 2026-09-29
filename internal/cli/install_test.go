package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ilias-t/bckp/internal/contracts"
	"github.com/ilias-t/bckp/internal/paths"
)

var (
	bootoutKey   = contracts.CommandLine(binLaunchctl, "bootout", "system/"+paths.DaemonLabel)
	pmsetZeroKey = contracts.CommandLine(paths.BinPmset, "-a", "disablesleep", "0")
)

const testPlist = "<plist>dev.bckp.helper</plist>\n"

// installEnv prepares a build dir with backpack (reached via a bckp
// symlink) and backpackd, and wires the plist.
func installEnv(t *testing.T) *testEnv {
	t.Helper()
	te := newTestEnv(t)
	build := t.TempDir()
	if err := os.WriteFile(filepath.Join(build, "backpack"), []byte("cli"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(build, "backpackd"), []byte("daemon-binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "bckp")
	if err := os.Symlink(filepath.Join(build, "backpack"), link); err != nil {
		t.Fatal(err)
	}
	te.executable = func() (string, error) { return link, nil }
	te.wiring.DaemonPlist = func() []byte { return []byte(testPlist) }
	return te
}

func (te *testEnv) asRoot(sudoUID string) {
	te.getuid = func() int { return 0 }
	te.environ = append(te.environ, "SUDO_UID="+sudoUID, "SUDO_USER=tester")
}

func (te *testEnv) bootstrapKey() string {
	return contracts.CommandLine(binLaunchctl, "bootstrap", "system", te.sys(paths.DaemonPlist))
}

func listFiles(t *testing.T, root string) []string {
	t.Helper()
	var files []string
	filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() {
			files = append(files, p)
		}
		return nil
	})
	return files
}

func TestInstallDryRunTouchesNothing(t *testing.T) {
	te := installEnv(t)
	if code := te.run("install", "--dry-run"); code != exitOK {
		t.Fatalf("code = %d stderr %q", code, te.errb)
	}
	if files := listFiles(t, te.root); len(files) != 0 {
		t.Fatalf("dry run wrote %v", files)
	}
	if calls := te.ex.Calls(); len(calls) != 0 {
		t.Fatalf("dry run ran %v", calls)
	}
	out := te.out.String()
	for _, want := range []string{
		"dry run",
		"would write", te.sys(paths.DaemonBinary) + " (13 bytes, root:wheel 0755)",
		te.sys(paths.AllowedUIDs) + " (4 bytes, root:wheel 0644)",
		te.sys(paths.DaemonPlist),
		"would run", bootoutKey, te.bootstrapKey(),
		"backpackd", // source resolved through the bckp symlink
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestInstallRefusals(t *testing.T) {
	te := installEnv(t)
	if code := te.run("install"); code != exitErr || !strings.Contains(te.errb.String(), "install needs root: sudo /") {
		t.Fatalf("non-root: code=%d stderr=%q", code, te.errb)
	}
	te = installEnv(t)
	te.getuid = func() int { return 0 }
	if code := te.run("install"); code != exitErr || !strings.Contains(te.errb.String(), "SUDO_UID") {
		t.Fatalf("no SUDO_UID: code=%d stderr=%q", code, te.errb)
	}
	te = installEnv(t)
	te.asRoot("0")
	if code := te.run("install"); code != exitErr || !strings.Contains(te.errb.String(), "non-root") {
		t.Fatalf("SUDO_UID=0: code=%d stderr=%q", code, te.errb)
	}
	te = installEnv(t)
	te.asRoot("501")
	te.wiring.DaemonPlist = nil
	if code := te.run("install"); code != exitErr || !strings.Contains(te.errb.String(), "not wired") {
		t.Fatalf("no plist: code=%d stderr=%q", code, te.errb)
	}
	te = installEnv(t)
	te.asRoot("501")
	if code := te.run("install", "--daemon-bin", "/nonexistent/backpackd"); code != exitErr {
		t.Fatalf("missing daemon: code=%d", code)
	}
	if files := listFiles(t, te.root); len(files) != 0 {
		t.Fatalf("refused install wrote %v", files)
	}
}

func TestInstallWritesVerifiesAndIsIdempotent(t *testing.T) {
	te := installEnv(t)
	te.asRoot("501")
	te.ex.OnFail(bootoutKey, 113, "Could not find service")
	// bootstrap right after bootout can fail transiently.
	te.ex.OnFail(te.bootstrapKey(), 5, "Bootstrap failed: 5: Input/output error")
	te.ex.On(te.bootstrapKey(), "")
	if code := te.run("install"); code != exitOK {
		t.Fatalf("code = %d stderr %q\n%s", code, te.errb, te.out)
	}
	check := func(p string, want string, mode os.FileMode) {
		t.Helper()
		data, err := os.ReadFile(p)
		if err != nil || string(data) != want {
			t.Fatalf("%s = %q, %v", p, data, err)
		}
		fi, _ := os.Stat(p)
		if fi.Mode().Perm() != mode {
			t.Fatalf("%s mode %v", p, fi.Mode())
		}
	}
	check(te.sys(paths.DaemonBinary), "daemon-binary", 0o755)
	check(te.sys(paths.AllowedUIDs), "501\n", 0o644)
	check(te.sys(paths.DaemonPlist), testPlist, 0o644)
	if fi, _ := os.Stat(te.sys(paths.SystemSupportDir)); fi.Mode().Perm() != 0o755 {
		t.Fatalf("support dir mode %v", fi.Mode())
	}
	if n := te.ex.CallCount(te.bootstrapKey()); n != 2 {
		t.Fatalf("bootstrap attempts = %d", n)
	}
	chowned := strings.Join(te.chowns, "\n")
	if !strings.Contains(chowned, te.sys(paths.SystemSupportDir)) || !strings.Contains(chowned, te.sys(daemonLogDir)) || len(te.chowns) != 5 {
		t.Fatalf("chowns = %v", te.chowns)
	}

	if fi, _ := os.Stat(te.sys(daemonLogDir)); fi == nil || !fi.IsDir() || fi.Mode().Perm() != 0o755 {
		t.Fatalf("daemon log dir: %v", fi)
	}

	// Second run: same content is unchanged; the allowlist holds only
	// SUDO_UID (backpackd parses it strictly).
	os.WriteFile(te.sys(paths.AllowedUIDs), []byte("502\n"), 0o644)
	te.out.Reset()
	if code := te.run("install"); code != exitOK {
		t.Fatalf("second run code = %d stderr %q", code, te.errb)
	}
	if !strings.Contains(te.out.String(), "unchanged") {
		t.Fatalf("second run output:\n%s", te.out)
	}
	check(te.sys(paths.AllowedUIDs), "501\n", 0o644)
	if !strings.Contains(te.out.String(), "previous allowlist [502] is replaced") {
		t.Fatalf("no replacement note:\n%s", te.out)
	}
}

func TestInstallFailsWhenOwnerDoesNotVerify(t *testing.T) {
	te := installEnv(t)
	te.asRoot("501")
	te.ownerOf = func(os.FileInfo) (int, int) { return 501, 20 }
	if code := te.run("install"); code != exitErr {
		t.Fatalf("code = %d", code)
	}
	if !strings.Contains(te.errb.String(), "owner is 501:20") {
		t.Fatalf("stderr = %q", te.errb)
	}
	if len(te.ex.Calls()) != 0 {
		t.Fatalf("launchctl ran after a failed verify: %v", te.ex.Calls())
	}
}

func TestUninstall(t *testing.T) {
	te := installEnv(t)
	te.asRoot("501")
	te.ex.On(bootoutKey, "")
	te.ex.On(te.bootstrapKey(), "")
	if code := te.run("install"); code != exitOK {
		t.Fatalf("install code = %d stderr %q", code, te.errb)
	}
	before := listFiles(t, te.root)

	// Dry run as a normal user changes nothing and runs nothing.
	te.getuid = func() int { return 501 }
	calls := len(te.ex.Calls())
	te.out.Reset()
	if code := te.run("uninstall", "--dry-run"); code != exitOK {
		t.Fatalf("dry code = %d stderr %q", code, te.errb)
	}
	if got := listFiles(t, te.root); len(got) != len(before) || len(te.ex.Calls()) != calls {
		t.Fatalf("dry run changed things: files %v calls %v", got, te.ex.Calls()[calls:])
	}
	for _, want := range []string{"would run", pmsetZeroKey, "would remove", te.sys(paths.DaemonBinary)} {
		if !strings.Contains(te.out.String(), want) {
			t.Errorf("dry output lacks %q:\n%s", want, te.out)
		}
	}

	if code := te.run("uninstall"); code != exitErr || !strings.Contains(te.errb.String(), "needs root") {
		t.Fatalf("non-root code = %d", code)
	}

	te.getuid = func() int { return 0 }
	te.ex.On(pmsetZeroKey, "")
	te.ex.On(ioregKey, ioregOut(false, true))
	te.out.Reset()
	if code := te.run("uninstall"); code != exitOK {
		t.Fatalf("code = %d stderr %q\n%s", code, te.errb, te.out)
	}
	if files := listFiles(t, te.root); len(files) != 0 {
		t.Fatalf("left behind %v", files)
	}
	if _, err := os.Stat(te.sys(paths.SystemSupportDir)); !os.IsNotExist(err) {
		t.Fatalf("support dir still there: %v", err)
	}
	all := te.ex.Calls()[calls:]
	if len(all) < 2 || all[0] != pmsetZeroKey || all[1] != bootoutKey {
		t.Fatalf("order = %v (pmset must come first)", all)
	}
	// Idempotent: a second uninstall skips missing files.
	te.out.Reset()
	if code := te.run("uninstall"); code != exitOK || !strings.Contains(te.out.String(), "not present") {
		t.Fatalf("second uninstall code = %d\n%s", code, te.out)
	}
}

func TestUninstallFailsWhenSleepStillDisabled(t *testing.T) {
	te := installEnv(t)
	te.getuid = func() int { return 0 }
	te.ex.OnFail(pmsetZeroKey, 1, "pmset: must be root")
	te.ex.OnFail(bootoutKey, 113, "not loaded")
	te.ex.On(ioregKey, ioregOut(true, true))
	if code := te.run("uninstall"); code != exitErr {
		t.Fatalf("code = %d", code)
	}
	if !strings.Contains(te.errb.String(), "SleepDisabled is still Yes") {
		t.Fatalf("stderr = %q", te.errb)
	}
}

func TestAllowlistFormat(t *testing.T) {
	if got := string(allowlistFor(501)); got != "501\n" {
		t.Fatalf("got %q", got)
	}
}

func TestInstallRefusesDirNotOwnedByRoot(t *testing.T) {
	te := installEnv(t)
	te.asRoot("501")
	support := te.sys(paths.SystemSupportDir)
	if err := os.MkdirAll(support, 0o755); err != nil {
		t.Fatal(err)
	}
	te.ownerOf = func(fi os.FileInfo) (int, int) {
		if fi.Name() == filepath.Base(support) {
			return 501, 20
		}
		return 0, 0
	}
	if code := te.run("install"); code != exitErr {
		t.Fatalf("code = %d\n%s", code, te.out)
	}
	if !strings.Contains(te.errb.String(), "owned by uid 501, not root") {
		t.Fatalf("stderr = %q", te.errb)
	}
	for _, p := range te.chowns {
		if p == support {
			t.Fatal("adopted a user-owned dir")
		}
	}
	if _, err := os.Stat(te.sys(paths.AllowedUIDs)); err == nil {
		t.Fatal("wrote the allowlist into a user-owned dir")
	}
	if len(te.ex.Calls()) != 0 {
		t.Fatalf("launchctl ran: %v", te.ex.Calls())
	}
}

func TestInstallFromTempDaemonBinAndHint(t *testing.T) {
	// install.sh runs the verified copy from its temp dir with
	// --daemon-bin; the helper is copied, so the temp path need not last.
	te := installEnv(t)
	te.asRoot("501")
	te.ex.On(bootoutKey, "")
	te.ex.On(te.bootstrapKey(), "")
	tmp := filepath.Join(t.TempDir(), "x", "backpackd")
	if err := os.MkdirAll(filepath.Dir(tmp), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tmp, []byte("temp-daemon"), 0o755); err != nil {
		t.Fatal(err)
	}
	if code := te.run("install", "--daemon-bin", tmp); code != exitOK {
		t.Fatalf("code = %d stderr %q", code, te.errb)
	}
	os.Remove(tmp)
	if b, err := os.ReadFile(te.sys(paths.DaemonBinary)); err != nil || string(b) != "temp-daemon" {
		t.Fatalf("installed %q, %v", b, err)
	}
	if !strings.Contains(te.out.String(), "Next: `backpack doctor`") {
		t.Fatalf("no doctor hint:\n%s", te.out)
	}

	// The start flow carries on by itself: no hint in the middle of it.
	te.out.Reset()
	if code := te.run("install", "--no-hint"); code != exitOK {
		t.Fatalf("code = %d stderr %q", code, te.errb)
	}
	if out := te.out.String(); strings.Contains(out, "Next:") || !strings.Contains(out, "installed backpackd for uid 501.") {
		t.Fatalf("--no-hint output:\n%s", out)
	}
}
