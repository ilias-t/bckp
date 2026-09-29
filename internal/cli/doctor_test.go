package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ilias-t/bckp/internal/contracts"
	"github.com/ilias-t/bckp/internal/fakes"
	"github.com/ilias-t/bckp/internal/paths"
	"github.com/ilias-t/bckp/internal/version"
)

var (
	dseditKey  = contracts.CommandLine(binDseditgroup, "-o", "checkmember", "-m", "tester", "admin")
	testTopicE = "BACKPACK_NOTIFY_NTFY_TOPIC=abc123topic"
)

func writeFile(t *testing.T, p, data string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(data), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
}

// healthyDoctorEnv is an install that passes every check.
func healthyDoctorEnv(t *testing.T) (*testEnv, *fakes.SleepGuard) {
	t.Helper()
	te := newTestEnv(t)
	writeFile(t, te.sys(paths.DaemonBinary), "bin", 0o755)
	writeFile(t, te.sys(paths.DaemonPlist), testPlist, 0o644)
	writeFile(t, te.sys(paths.AllowedUIDs), "501\n", 0o644)
	writeFile(t, te.sys(paths.MacmonCandidates[0]), "macmon", 0o755)
	guard := fakes.NewSleepGuard(nil)
	te.wiring.NewGuard = func() contracts.SleepGuard { return guard }
	te.environ = []string{testTopicE}
	writeFile(t, te.user.ConfigFile, "[session]\n", 0o600)
	te.ex.On(dseditKey, "yes tester is a member of admin\n")
	te.ex.On(ioregKey, ioregOut(false, true))
	return te, guard
}

func TestDoctorAllGood(t *testing.T) {
	te, _ := healthyDoctorEnv(t)
	if code := te.run("doctor"); code != exitOK {
		t.Fatalf("code = %d\n%s%s", code, te.out, te.errb)
	}
	out := te.out.String()
	if strings.Contains(out, "✗") || !strings.Contains(out, "all good.") {
		t.Fatalf("output:\n%s", out)
	}
	for _, want := range []string{"✓ daemon binary", "✓ daemon        responding (no lease, backstop idle)", "✓ admin group", "✓ macmon", "✓ alerts        macOS notifications and ntfy (ab*******ic)", "✓ kernel"} {
		if !strings.Contains(out, want) {
			t.Errorf("lacks %q:\n%s", want, out)
		}
	}
}

// macmon and ntfy are optional: without them doctor still passes, with
// "i" tips that are not counted as warnings.
func TestDoctorOptionalExtrasAreTips(t *testing.T) {
	te, _ := healthyDoctorEnv(t)
	te.environ = nil
	if err := os.Remove(te.sys(paths.MacmonCandidates[0])); err != nil {
		t.Fatal(err)
	}
	if code := te.run("doctor"); code != exitOK {
		t.Fatalf("code = %d\n%s", code, te.out)
	}
	out := te.out.String()
	for _, want := range []string{
		"backpack doctor (" + version.String() + ")\n",
		"  i macmon        optional, not installed (thermal state only)\n                  tip: brew install macmon for °C readings\n",
		"  i alerts        macOS notifications only\n                  tip: add an ntfy topic for your phone",
		"all good.\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "✗") || strings.Contains(out, "warning") {
		t.Fatalf("output:\n%s", out)
	}
}

func TestDoctorNoAlertBackendIsAWarning(t *testing.T) {
	te, _ := healthyDoctorEnv(t)
	te.environ = []string{"BACKPACK_NOTIFY_MACOS=false"}
	if code := te.run("doctor"); code != exitOK {
		t.Fatalf("code = %d\n%s", code, te.out)
	}
	if out := te.out.String(); !strings.Contains(out, "! alerts        notify.macos is off and notify.ntfy_topic is not set") ||
		!strings.Contains(out, "all good, 1 warning(s).") {
		t.Fatalf("output:\n%s", out)
	}
}

func TestDoctorMissingConfigIsAWarning(t *testing.T) {
	te, _ := healthyDoctorEnv(t)
	if err := os.Remove(te.user.ConfigFile); err != nil {
		t.Fatal(err)
	}
	if code := te.run("doctor"); code != exitOK {
		t.Fatalf("code = %d\n%s", code, te.out)
	}
	out := te.out.String()
	want := "! config        " + te.user.ConfigFile + " not created (defaults in use)"
	if !strings.Contains(out, want) || strings.Contains(out, "✓ config") || !strings.Contains(out, "all good, 1 warning(s).") {
		t.Fatalf("want %q in:\n%s", want, out)
	}
}

func TestDoctorHotspotOnWiFiNote(t *testing.T) {
	te, _ := healthyDoctorEnv(t)
	writeFile(t, te.user.ConfigFile, "[network]\nprefer_hotspot = \"on\"\n", 0o600)
	if code := te.run("doctor"); code != exitOK {
		t.Fatalf("code = %d\n%s", code, te.out)
	}
	out := te.out.String()
	want := "! wifi hotspot  for Wi-Fi hotspot fallback set System Settings → Wi-Fi → Ask to join hotspots → Automatic"
	if !strings.Contains(out, want) || !strings.Contains(out, "all good, 1 warning(s).") {
		t.Fatalf("want %q in:\n%s", want, out)
	}
	for _, cfg := range []string{"[network]\nprefer_hotspot = \"auto\"\n", "[network]\nprefer_hotspot = \"on\"\nwifi_nudge_after = \"0s\"\n"} {
		te, _ := healthyDoctorEnv(t)
		writeFile(t, te.user.ConfigFile, cfg, 0o600)
		te.run("doctor")
		if strings.Contains(te.out.String(), "wifi hotspot") {
			t.Fatalf("%q: note shown:\n%s", cfg, te.out)
		}
	}
}

func TestDoctorReportsProblems(t *testing.T) {
	te := newTestEnv(t)
	te.ex.OnFail(dseditKey, 67, "")
	te.ex.On(ioregKey, ioregOut(true, true))
	writeFile(t, te.user.RestoreJournal, "{}", 0o600)
	if code := te.run("doctor"); code != exitErr {
		t.Fatalf("code = %d\n%s", code, te.out)
	}
	out := te.out.String()
	for _, want := range []string{
		"✗ daemon binary", "is missing", "fix: sudo /nonexistent/backpack install",
		"✗ daemon        daemon client not wired",
		"✗ admin group",
		"i macmon        optional, not installed (thermal state only)", "tip: brew install macmon for °C readings",
		"i alerts        macOS notifications only", "tip: add an ntfy topic for your phone",
		"✗ net journal", "backpack doctor --fix",
		"✗ kernel        SleepDisabled=Yes with no lease", "sudo pmset -a disablesleep 0",
		"problem(s)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("lacks %q:\n%s", want, out)
		}
	}
}

func TestDoctorFileOwnershipAndFormat(t *testing.T) {
	te, _ := healthyDoctorEnv(t)
	os.Chmod(te.sys(paths.DaemonPlist), 0o666)
	writeFile(t, te.sys(paths.AllowedUIDs), "501 \n", 0o644)
	te.ownerOf = func(fi os.FileInfo) (int, int) {
		if fi.Name() == filepath.Base(paths.DaemonBinary) {
			return 501, 20
		}
		return 0, 0
	}
	if code := te.run("doctor"); code != exitErr {
		t.Fatalf("code = %d", code)
	}
	out := te.out.String()
	for _, want := range []string{"owned by 501:20, want root:wheel", "has mode 0666, want 0644", "not a bare decimal uid"} {
		if !strings.Contains(out, want) {
			t.Errorf("lacks %q:\n%s", want, out)
		}
	}
}

func TestDoctorUIDNotAllowed(t *testing.T) {
	te, _ := healthyDoctorEnv(t)
	writeFile(t, te.sys(paths.AllowedUIDs), "502\n", 0o644)
	if code := te.run("doctor"); code != exitErr || !strings.Contains(te.out.String(), "uid 501 is not in") {
		t.Fatalf("code = %d\n%s", code, te.out)
	}
}

func TestDoctorDaemonDown(t *testing.T) {
	te, guard := healthyDoctorEnv(t)
	guard.SetStatus(nil, contracts.ErrDaemonUnavailable)
	if code := te.run("doctor"); code != exitErr || !strings.Contains(te.out.String(), "✗ daemon        no answer") {
		t.Fatalf("code = %d\n%s", code, te.out)
	}
}

func TestDoctorSleepDisabledWithLeaseIsFine(t *testing.T) {
	te, guard := healthyDoctorEnv(t)
	te.ex = fakes.NewExec()
	te.exec = te.ex
	te.ex.On(dseditKey, "yes tester is a member of admin\n")
	te.ex.On(ioregKey, ioregOut(true, true))
	if _, err := guard.Acquire(context.Background(), 0, "test"); err != nil {
		t.Fatal(err)
	}
	if code := te.run("doctor"); code != exitOK {
		t.Fatalf("code = %d\n%s", code, te.out)
	}
}

func TestDoctorFixRestoresJournal(t *testing.T) {
	te, _ := healthyDoctorEnv(t)
	writeFile(t, te.user.RestoreJournal, "{}", 0o600)
	called := false
	te.wiring.RestoreJournal = func(context.Context) (bool, error) { called = true; return true, nil }
	if code := te.run("doctor", "--fix"); code != exitOK || !called {
		t.Fatalf("code = %d called=%v\n%s", code, called, te.out)
	}
	if !strings.Contains(te.out.String(), "✓ net journal   restored") {
		t.Fatalf("output:\n%s", te.out)
	}
}

func TestDoctorAsRootSkipsSocket(t *testing.T) {
	te, _ := healthyDoctorEnv(t)
	te.getuid = func() int { return 0 }
	te.environ = append(te.environ, "SUDO_UID=501")
	te.wiring.NewGuard = func() contracts.SleepGuard { t.Fatal("must not dial the daemon as root"); return nil }
	if code := te.run("doctor"); code != exitOK {
		t.Fatalf("code = %d\n%s", code, te.out)
	}
	if !strings.Contains(te.out.String(), "! daemon        running as root") {
		t.Fatalf("output:\n%s", te.out)
	}
}

func TestStrictAllowlist(t *testing.T) {
	for in, ok := range map[string]bool{
		"501\n": true, "501": true, "501\n502\n": true,
		"": false, "\n": false, "501\n\n": false, " 501\n": false, "501\r\n": false, "# c\n501\n": false, "abc\n": false,
	} {
		if err := strictAllowlistErr([]byte(in)); (err == nil) != ok {
			t.Errorf("%q: err = %v", in, err)
		}
	}
}
