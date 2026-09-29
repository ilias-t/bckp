//go:build darwin

package engine

import (
	"os/exec"
	"strconv"
	"strings"
	"testing"
)

// TestSysLoadAvgMatchesSysctl checks the vm.loadavg struct layout against
// the sysctl tool's own rendering.
func TestSysLoadAvgMatchesSysctl(t *testing.T) {
	got, err := sysLoadAvg()
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("/usr/sbin/sysctl", "-n", "vm.loadavg").Output()
	if err != nil {
		t.Skipf("sysctl: %v", err)
	}
	f := strings.Fields(strings.Trim(strings.TrimSpace(string(out)), "{}"))
	want, err := strconv.ParseFloat(f[0], 64)
	if err != nil {
		t.Fatalf("parse %q: %v", out, err)
	}
	if d := got - want; d > 0.5 || d < -0.5 {
		t.Fatalf("sysLoadAvg = %.2f, sysctl says %.2f", got, want)
	}
}
