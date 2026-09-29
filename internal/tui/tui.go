// Package tui is backpack's live dashboard: a bubbletea program that
// subscribes to the engine (contracts.EngineClient) and renders the session,
// heat, power, network and recent notifications.
//
// Rendering is a pure function of the model (render in render.go) so it is
// tested with golden files, without a terminal.
package tui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/muesli/termenv"

	"github.com/ilias-t/bckp/internal/contracts"
)

// DetachedMessage is printed after `q` detaches the TUI, and after an
// external interrupt (SIGINT/SIGTERM) or ctx cancellation ends an attach.
const DetachedMessage = "still running — `bckp attach` to return"

// ForegroundQuitText is the confirmation `q` shows in a foreground
// session (`backpack -f`): the engine runs inside this process, so leaving
// the TUI would end the session, and q cannot detach.
const ForegroundQuitText = "Foreground session (-f): q stops it. Stop now? [y/N]"

// Options configure Run.
type Options struct {
	// Foreground is set when the engine runs in this process (`-f`):
	// q asks to stop the session instead of detaching, and an interrupt
	// prints nothing (the caller stops the engine).
	Foreground bool
}

// Run shows the dashboard for the engine behind c until the user detaches
// (q), the session ends and the user presses a key, or the TUI is
// interrupted from outside: ctx cancelled (the CLI cancels it on SIGINT,
// SIGTERM and SIGHUP) or a SIGINT seen by bubbletea itself. Keyboard
// Ctrl-C is not an interrupt; in the TUI it asks to stop the session.
//
// It subscribes once before starting the UI and returns that error (for
// example one matching contracts.ErrEngineNotRunning) if it fails. Later
// drops are retried with backoff inside the UI. A detach, and an interrupt
// of an attached (non-foreground) TUI, leave the engine running and print
// DetachedMessage to stdout. Run returns nil in all of these cases.
func Run(ctx context.Context, c contracts.EngineClient, o Options) error {
	return run(ctx, c, os.Stdin, os.Stdout, o)
}

func run(ctx context.Context, c contracts.EngineClient, in io.Reader, out io.Writer, o Options) error {
	subCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	ch, err := c.Subscribe(subCtx)
	if err != nil {
		return fmt.Errorf("subscribe to engine: %w", err)
	}

	st := newStyles(detectProfile(out))
	m := newModel(ctx, c, ch, cancel, st)
	m.foreground = o.Foreground
	p := tea.NewProgram(m,
		tea.WithContext(ctx),
		tea.WithInput(in),
		tea.WithOutput(out),
		tea.WithAltScreen(),
	)
	final, err := p.Run()
	if err != nil {
		// Interrupted from outside. bubbletea and the CLI's signal ctx
		// both see a SIGINT, in either order, so both forms count.
		interrupted := errors.Is(err, tea.ErrInterrupted) || (errors.Is(err, tea.ErrProgramKilled) && ctx.Err() != nil)
		if !interrupted {
			return err
		}
		if !o.Foreground {
			fmt.Fprintln(out, DetachedMessage)
		}
		return nil
	}
	fm, _ := final.(model)
	fm.cancelSub()
	if line := exitLine(fm); line != "" {
		fmt.Fprintln(out, line)
	}
	return nil
}

// exitLine is printed to the normal screen after the TUI exits.
func exitLine(m model) string {
	switch m.exit {
	case exitDetached:
		return DetachedMessage
	case exitEnded:
		if m.snap != nil {
			return "session ended: " + endReasonText(m.snap.Session.EndReason)
		}
		return "session ended"
	case exitLost:
		return "disconnected — the engine may still be running; `bckp status` to check"
	}
	return ""
}

// detectProfile returns the color profile for out, honoring NO_COLOR and
// CLICOLOR. Non-terminals get termenv.Ascii (no escapes).
func detectProfile(out io.Writer, opts ...termenv.OutputOption) termenv.Profile {
	return termenv.NewOutput(out, opts...).EnvColorProfile()
}
