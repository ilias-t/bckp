package client

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ilias-t/bckp/internal/paths"
)

// End-to-end dead-man's switch against the installed backpackd. Runs only
// with BACKPACK_E2E=1, after `sudo backpack install`. It really disables
// sleep for a moment.

const holderEnv = "BACKPACK_E2E_HOLDER"

var sleepDisabledRE = regexp.MustCompile(`"SleepDisabled" = (Yes|No)\b`)

func kernelSleepDisabled(t *testing.T) bool {
	t.Helper()
	out, err := exec.Command(paths.BinIoreg, "-r", "-k", "AppleClamshellState", "-d", "4").Output()
	if err != nil {
		t.Fatalf("ioreg: %v", err)
	}
	m := sleepDisabledRE.FindSubmatch(out)
	if m == nil {
		t.Fatal("SleepDisabled not in ioreg output")
	}
	return string(m[1]) == "Yes"
}

// TestE2EHolder is the child process: it acquires a lease, reports it, and
// waits to be killed.
func TestE2EHolder(t *testing.T) {
	if os.Getenv(holderEnv) != "1" {
		t.Skip("helper process for TestE2EKillHolderResets")
	}
	if _, err := New(paths.DaemonSocket).Acquire(context.Background(), 0, "e2e kill -9 test"); err != nil {
		os.Stdout.WriteString("error: " + err.Error() + "\n")
		os.Exit(1)
	}
	os.Stdout.WriteString("acquired\n")
	time.Sleep(time.Minute)
}

func TestE2EKillHolderResets(t *testing.T) {
	if os.Getenv("BACKPACK_E2E") != "1" {
		t.Skip("set BACKPACK_E2E=1 to run against the installed backpackd")
	}
	if kernelSleepDisabled(t) {
		t.Fatal("SleepDisabled is already Yes; reset it (backpack doctor) before running")
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestE2EHolder$", "-test.count=1")
	cmd.Env = append(os.Environ(), holderEnv+"=1")
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()
	lines := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(out)
		for sc.Scan() {
			if l := sc.Text(); l == "acquired" || strings.HasPrefix(l, "error:") {
				lines <- l
				return
			}
		}
		lines <- "holder exited"
	}()
	select {
	case l := <-lines:
		if l != "acquired" {
			t.Fatalf("holder: %s", l)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("holder did not acquire a lease")
	}
	if !kernelSleepDisabled(t) {
		t.Fatal("SleepDisabled not Yes while the lease is held")
	}
	if err := cmd.Process.Kill(); err != nil { // SIGKILL
		t.Fatal(err)
	}
	killed := time.Now()
	_ = cmd.Wait()
	for kernelSleepDisabled(t) {
		if time.Since(killed) > 2*time.Second {
			t.Fatal("SleepDisabled still Yes 2s after kill -9 of the lease holder")
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Logf("SleepDisabled=No %v after kill -9", time.Since(killed).Round(time.Millisecond))
}
