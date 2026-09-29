// Command backpackd is the root LaunchDaemon that owns `pmset -a
// disablesleep`. All logic lives in internal/daemon; see that package for
// the invariants.
package main

import (
	"os"

	"github.com/ilias-t/bckp/internal/daemon"
	"github.com/ilias-t/bckp/internal/paths"
	"github.com/ilias-t/bckp/internal/providers/power"
	"github.com/ilias-t/bckp/internal/providers/thermal"
	"github.com/ilias-t/bckp/internal/system"
)

func main() {
	deps := daemon.Deps{
		Exec:  system.Exec{},
		Clock: system.Clock{},
		// Main opens the JSONL event log here (readable by the user for
		// `backpack logs`), falling back to stderr if it can't.
		LogDir: paths.DaemonLogDir,
		// The backstop sources run as root, so they only use the absolute
		// system tool paths in internal/paths. DisableMacmon keeps the
		// thermal source to the in-process cgo shim: root must never run
		// macmon, which lives in user-writable Homebrew prefixes (macmon.Read
		// also refuses to run as root).
		Thermal: thermal.New(system.Exec{}, thermal.Options{DisableMacmon: true}),
		Power:   power.New(system.Exec{}),
	}
	os.Exit(daemon.Main(deps))
}
