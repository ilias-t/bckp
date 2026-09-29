// Package system provides the real implementations of contracts.Clock and
// contracts.Exec. Tests use internal/fakes instead.
package system

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/ilias-t/bckp/internal/contracts"
)

// Clock is the wall clock.
type Clock struct{}

var _ contracts.Clock = Clock{}

func (Clock) Now() time.Time                         { return time.Now() }
func (Clock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// Exec runs commands with os/exec, following the contracts.Exec error
// contract. It never uses a shell. Commands run with LC_ALL=C so output is
// parseable regardless of the user's locale.
type Exec struct{}

var _ contracts.Exec = Exec{}

// waitDelay bounds how long Run waits for output pipes after ctx ends.
const waitDelay = 2 * time.Second

func (Exec) Run(ctx context.Context, name string, args ...string) (contracts.ExecResult, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	cmd.WaitDelay = waitDelay
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	res := contracts.ExecResult{Stdout: stdout.Bytes(), Stderr: stderr.Bytes(), ExitCode: -1}
	line := contracts.CommandLine(name, args...)
	if ctxErr := ctx.Err(); ctxErr != nil && err != nil {
		return res, fmt.Errorf("%s: %w", line, ctxErr)
	}
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		res.ExitCode = 0
		return res, nil
	case errors.As(err, &exitErr) && exitErr.Exited():
		res.ExitCode = exitErr.ExitCode()
		return res, &contracts.ExitError{Cmd: line, ExitCode: res.ExitCode, Stderr: res.Stderr}
	default:
		return res, fmt.Errorf("%s: %w", line, err)
	}
}
