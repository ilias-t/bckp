package system

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/ilias-t/bckp/internal/contracts"
)

func TestExecSuccess(t *testing.T) {
	res, err := Exec{}.Run(context.Background(), "/bin/echo", "hello")
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 0 || strings.TrimSpace(string(res.Stdout)) != "hello" {
		t.Fatalf("res = %+v", res)
	}
}

func TestExecNonZeroExit(t *testing.T) {
	res, err := Exec{}.Run(context.Background(), "/bin/sh", "-c", "echo out; echo oops >&2; exit 3")
	var ee *contracts.ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("err = %v, want *ExitError", err)
	}
	if ee.ExitCode != 3 || res.ExitCode != 3 {
		t.Errorf("exit = %d / %d", ee.ExitCode, res.ExitCode)
	}
	if strings.TrimSpace(string(res.Stdout)) != "out" || !strings.Contains(ee.Error(), "oops") {
		t.Errorf("res = %+v, err = %v", res, ee)
	}
}

func TestExecNotFound(t *testing.T) {
	res, err := Exec{}.Run(context.Background(), "/nonexistent/bckp-tool")
	if err == nil || res.ExitCode != -1 {
		t.Fatalf("res = %+v, err = %v", res, err)
	}
	var ee *contracts.ExitError
	if errors.As(err, &ee) {
		t.Fatal("start failure must not be an ExitError")
	}
	if !errors.Is(err, exec.ErrNotFound) && !strings.Contains(err.Error(), "no such file") {
		t.Errorf("err = %v", err)
	}
}

func TestExecContextCancel(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	res, err := Exec{}.Run(ctx, "/bin/sleep", "5")
	if !errors.Is(err, context.DeadlineExceeded) || res.ExitCode != -1 {
		t.Fatalf("res = %+v, err = %v", res, err)
	}
}

func TestClock(t *testing.T) {
	c := Clock{}
	before := time.Now()
	if c.Now().Before(before) {
		t.Fatal("Now went backwards")
	}
	select {
	case <-c.After(time.Millisecond):
	case <-time.After(time.Second):
		t.Fatal("After did not fire")
	}
}
