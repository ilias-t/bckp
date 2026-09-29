package contracts

import (
	"context"
	"fmt"
	"strings"
)

// Exec runs an external command. Every provider shells out through it so
// tests can substitute fakes.Exec with recorded output.
//
// Callers pass absolute paths for system tools (see internal/paths Bin*
// constants) so fake keys and real invocations match exactly.
//
// Error contract:
//   - Exit status 0: err is nil. Stdout and Stderr are both returned,
//     because some tools (networksetup) exit 0 on failure and report it in
//     their output; callers must parse it.
//   - Non-zero exit: the result holds the captured output and ExitCode, and
//     err is an *ExitError.
//   - The command could not start, or ctx ended: ExitCode is -1 and err
//     describes the cause (wrapping ctx.Err() when ctx ended).
type Exec interface {
	Run(ctx context.Context, name string, args ...string) (ExecResult, error)
}

// ExecResult is the captured outcome of one command.
type ExecResult struct {
	Stdout   []byte
	Stderr   []byte
	ExitCode int
}

// ExitError reports a non-zero exit status.
type ExitError struct {
	// Cmd is CommandLine(name, args...).
	Cmd      string
	ExitCode int
	Stderr   []byte
}

func (e *ExitError) Error() string {
	msg := strings.TrimSpace(string(e.Stderr))
	if msg == "" {
		return fmt.Sprintf("%s: exit status %d", e.Cmd, e.ExitCode)
	}
	return fmt.Sprintf("%s: exit status %d: %s", e.Cmd, e.ExitCode, msg)
}

// CommandLine renders a command as name followed by args, joined with
// single spaces and no quoting: CommandLine("/usr/bin/pmset", "-g", "batt")
// == "/usr/bin/pmset -g batt". It is the key format fakes.Exec matches on and
// the form used in logs.
func CommandLine(name string, args ...string) string {
	if len(args) == 0 {
		return name
	}
	return name + " " + strings.Join(args, " ")
}
