package fakes

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/ilias-t/bckp/internal/contracts"
)

// ErrUnexpectedCommand is returned (wrapped) by Exec.Run for a command
// line with no registered response. Unmatched commands never succeed
// silently.
var ErrUnexpectedCommand = errors.New("fakes: unexpected command")

// ExecResponse is one canned answer.
type ExecResponse struct {
	Result contracts.ExecResult
	Err    error
}

// Exec is a fake contracts.Exec keyed on the exact command line as built
// by contracts.CommandLine(name, args...), e.g.
// "/usr/sbin/networksetup -listnetworkserviceorder".
//
// Responses registered for the same key are returned in order; the last
// one repeats forever. Every call is recorded, matched or not.
type Exec struct {
	mu        sync.Mutex
	responses map[string][]ExecResponse
	calls     []string
}

var _ contracts.Exec = (*Exec)(nil)

// NewExec returns an Exec with no responses.
func NewExec() *Exec {
	return &Exec{responses: map[string][]ExecResponse{}}
}

// On queues a successful response with the given stdout (exit 0).
func (e *Exec) On(cmdline, stdout string) *Exec {
	return e.OnResponse(cmdline, ExecResponse{Result: contracts.ExecResult{Stdout: []byte(stdout)}})
}

// OnFail queues a non-zero exit with the given stderr; Run returns a
// *contracts.ExitError, as the real runner does.
func (e *Exec) OnFail(cmdline string, exitCode int, stderr string) *Exec {
	res := contracts.ExecResult{Stderr: []byte(stderr), ExitCode: exitCode}
	return e.OnResponse(cmdline, ExecResponse{
		Result: res,
		Err:    &contracts.ExitError{Cmd: cmdline, ExitCode: exitCode, Stderr: res.Stderr},
	})
}

// OnResponse queues an arbitrary response.
func (e *Exec) OnResponse(cmdline string, r ExecResponse) *Exec {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.responses[cmdline] = append(e.responses[cmdline], r)
	return e
}

// Run returns the next canned response for the command line.
func (e *Exec) Run(ctx context.Context, name string, args ...string) (contracts.ExecResult, error) {
	line := contracts.CommandLine(name, args...)
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls = append(e.calls, line)
	if err := ctx.Err(); err != nil {
		return contracts.ExecResult{ExitCode: -1}, fmt.Errorf("%s: %w", line, err)
	}
	q := e.responses[line]
	if len(q) == 0 {
		return contracts.ExecResult{ExitCode: -1}, fmt.Errorf("%w: %q", ErrUnexpectedCommand, line)
	}
	r := q[0]
	if len(q) > 1 {
		e.responses[line] = q[1:]
	}
	return r.Result, r.Err
}

// Calls returns every command line run so far, in order.
func (e *Exec) Calls() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.calls...)
}

// CallCount returns how many times cmdline was run.
func (e *Exec) CallCount(cmdline string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	n := 0
	for _, c := range e.calls {
		if c == cmdline {
			n++
		}
	}
	return n
}
