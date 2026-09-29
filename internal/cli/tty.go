package cli

import (
	"os"

	"golang.org/x/sys/unix"
)

// isTerminal reports whether f is a terminal. It asks the tty driver
// (TIOCGETA) rather than checking for a character device, because
// /dev/null is a character device too and an agent's Bash tool may hand
// us /dev/null on stdin; mistaking that for a TTY would attach the TUI
// and hang the caller.
func isTerminal(f *os.File) bool {
	if f == nil {
		return false
	}
	_, err := unix.IoctlGetTermios(int(f.Fd()), unix.TIOCGETA)
	return err == nil
}
