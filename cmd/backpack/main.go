// Command backpack keeps a lid-closed MacBook awake, watches its heat, and
// keeps the iPhone hotspot link healthy. `bckp` is a symlink to it.
package main

import (
	"context"
	"os"

	"github.com/ilias-t/bckp/internal/cli"
	"github.com/ilias-t/bckp/internal/contracts"
	"github.com/ilias-t/bckp/internal/daemon"
	"github.com/ilias-t/bckp/internal/daemon/client"
	"github.com/ilias-t/bckp/internal/engine"
	"github.com/ilias-t/bckp/internal/paths"
	"github.com/ilias-t/bckp/internal/providers/network"
	"github.com/ilias-t/bckp/internal/simulate"
	"github.com/ilias-t/bckp/internal/system"
	"github.com/ilias-t/bckp/internal/tui"
)

// wiring connects the engine, daemon client, providers, notifiers and TUI
// to the CLI, which imports none of them itself.
var wiring = cli.Wiring{
	NewGuard: func() contracts.SleepGuard { return client.New(paths.DaemonSocket) },
	// `notify test` must see each backend's real delivery result, so
	// these have no outbox.
	NewNotifiers: namedNotifiers,
	RunEngine:    runEngine,
	// The journal names the services and Wi-Fi state it changed, so a
	// default-configured Network can restore it.
	RestoreJournal: func(ctx context.Context) (bool, error) {
		u, err := paths.Current()
		if err != nil {
			return false, err
		}
		return engine.RestoreStaleJournal(ctx, network.New(system.Exec{}, network.Options{}), u)
	},
	Simulate:        simulate.Run,
	SimulateGrammar: simulate.Grammar,
	DaemonPlist:     daemon.LaunchdPlist,
	RunTUI:          runTUI,
}

// runTUI runs the dashboard. While it owns the terminal, an engine running
// in this process (`backpack -f`) logs to the engine log file instead of
// stderr, so log lines don't land on the dashboard.
func runTUI(ctx context.Context, c contracts.EngineClient, o cli.TUIOptions) error {
	engineLogSink.setTUI(true)
	defer engineLogSink.setTUI(false)
	return tui.Run(ctx, c, tui.Options{Foreground: o.Foreground})
}

func main() {
	os.Exit(cli.Main(os.Args, wiring))
}
