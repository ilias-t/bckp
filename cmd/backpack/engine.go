package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/ilias-t/bckp/internal/cli"
	"github.com/ilias-t/bckp/internal/config"
	"github.com/ilias-t/bckp/internal/contracts"
	"github.com/ilias-t/bckp/internal/daemon/client"
	"github.com/ilias-t/bckp/internal/engine"
	"github.com/ilias-t/bckp/internal/engine/ipc"
	"github.com/ilias-t/bckp/internal/engineclient"
	"github.com/ilias-t/bckp/internal/eventlog"
	"github.com/ilias-t/bckp/internal/paths"
	"github.com/ilias-t/bckp/internal/providers/network"
	"github.com/ilias-t/bckp/internal/providers/power"
	"github.com/ilias-t/bckp/internal/providers/thermal"
	"github.com/ilias-t/bckp/internal/system"
)

// stopTimeout bounds the graceful stop after SIGINT/SIGTERM/SIGHUP: the
// loop picking up the stop, restoring the network, flushing notifications
// and releasing the lease. Past it, the session ctx is cancelled.
const stopTimeout = 45 * time.Second

// probeTimeout bounds the check for an engine behind an existing socket.
const probeTimeout = 2 * time.Second

// signalRace is how long a cancelled ctx waits for the signal behind it.
const signalRace = 50 * time.Millisecond

// runEngine is cli.Wiring.RunEngine: it builds the real providers and runs
// one session, serving the engine protocol on paths.User.EngineSocket.
func runEngine(ctx context.Context, cfg config.Config, opts cli.EngineOptions) error {
	u, err := paths.Current()
	if err != nil {
		return err
	}
	engineLogSink.setFile(u.EngineLog)
	// Events go to the rotating JSONL log that `backpack logs` reads. The
	// same records go to engineLogSink only when it can't open.
	logOpts := eventlog.Options{
		Src:    eventlog.SrcEngine,
		Redact: []string{cfg.Notify.NtfyTopic},
	}.WithConfig(cfg.Log.MaxMB, cfg.Log.MaxAge)
	logger, closer, err := eventlog.Open(u.LogDir, eventlog.EngineName, logOpts)
	if err != nil {
		// Same records (and redaction), on the engine log instead.
		logger = slog.New(eventlog.NewHandler(engineLogSink, logOpts)).With("src", eventlog.SrcEngine)
		logger.Warn("event log unavailable; logging to engine.log", "err", err)
	} else {
		defer closer.Close()
	}

	exec := system.Exec{}
	deps := engine.Deps{
		Clock: system.Clock{},
		// MacmonPath "" resolves macmon from paths.MacmonCandidates on each
		// read (the engine runs as the user, never root).
		Thermal: thermal.New(exec, thermal.Options{}),
		Power:   power.New(exec),
		Network: network.New(exec, network.Options{
			Service:    cfg.Network.Service,
			Iface:      cfg.Network.Iface,
			Gateway:    cfg.Network.Gateway,
			WiFiIface:  cfg.Network.WiFiIface,
			NtfyServer: cfg.Notify.NtfyServer,
		}),
		// The outbox (with ntfy) makes it a contracts.Outbox.
		Notifier: notifierChain(cfg, u.OutboxDir),
		Guard:    client.New(paths.DaemonSocket),
		Config:   cfg,
		Paths:    u,
		Logger:   logger,
	}
	// The hotspot preference and watchdog are already in cfg.Network; a
	// forever session has a zero Duration.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sigs)
	return serveSession(ctx, sigs, engine.New(deps), engine.Options{
		Duration: opts.Duration,
		Choices:  opts.Choices,
	}, u, logger)
}

// serveSession runs e until the session ends, serving the engine protocol
// on u.EngineSocket meanwhile.
//
// Cancelling ctx (the CLI cancels it on SIGINT/SIGTERM/SIGHUP, and when the
// TUI of a foreground session exits) stops the session like `backpack
// stop` does: e never sees ctx itself, only a Stop. A SIGTERM or SIGHUP on
// sigs (logout, kill, a closed terminal: nobody asked backpack to stop)
// ends it as EndSignal, which needs attention; anything else as
// EndStopped. The session ctx is cancelled only if that stop does not
// finish within stopTimeout.
//
// Shutdown order: Run returns first (the session has fully ended), then
// Serve is cancelled and awaited. The `stop` request that ended the
// session may still be in flight then; Serve keeps its ctx live (up to
// ipc.ShutdownGrace) so it is answered with success, not "context
// canceled". The socket is removed last.
func serveSession(ctx context.Context, sigs <-chan os.Signal, e *engine.Engine, opts engine.Options, u paths.User, log *slog.Logger) error {
	unlock, err := lockEngine(u.EngineLock)
	if err != nil {
		return err
	}
	defer unlock()
	ln, err := listenEngine(u.EngineSocket)
	if err != nil {
		return err
	}
	defer removeSocket(u.EngineSocket)

	serveCtx, cancelServe := context.WithCancel(context.Background())
	defer cancelServe()
	served := make(chan error, 1)
	go func() { served <- ipc.Serve(serveCtx, ln, e) }()

	runCtx, cancelRun := context.WithCancel(context.Background())
	defer cancelRun()
	watched := make(chan struct{})
	go func() {
		defer close(watched)
		var sig os.Signal
		select {
		case sig = <-sigs:
		case <-ctx.Done():
			// The CLI's own handler may cancel ctx a moment before sigs
			// receives the same signal.
			select {
			case sig = <-sigs:
			case <-time.After(signalRace):
			}
		case <-e.Done():
			return
		}
		sctx, cancel := context.WithTimeout(context.Background(), stopTimeout)
		defer cancel()
		if err := stopEngine(sctx, e, signalName(sig)); err != nil {
			log.Warn("graceful stop failed; cancelling the session", "err", err)
			cancelRun()
		}
	}()
	runErr := e.Run(runCtx, opts)
	cancelRun()
	<-watched

	cancelServe()
	if err := <-served; err != nil {
		log.Error("engine socket stopped serving", "err", err)
	}
	return runErr
}

// signalName names a signal that ends a session as EndSignal ("SIGTERM"),
// or returns "" for a user stop.
func signalName(sig os.Signal) string {
	switch sig {
	case syscall.SIGTERM:
		return "SIGTERM"
	case syscall.SIGHUP:
		return "SIGHUP"
	}
	return ""
}

// stopEngine asks e to stop the session (for sigName, when not "") and
// waits for it to end. Stop returns nil once the session has ended,
// including when it had already ended (the TUI stopped it first). A stop
// that races Run's start gets ErrEngineNotRunning; it is retried until Run
// accepts it.
func stopEngine(ctx context.Context, e *engine.Engine, sigName string) error {
	for {
		var err error
		if sigName != "" {
			err = e.StopForSignal(ctx, sigName)
		} else {
			err = e.Stop(ctx, nil)
		}
		if !errors.Is(err, contracts.ErrEngineNotRunning) {
			return err
		}
		select {
		case <-e.Done():
			return nil
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// lockEngine takes the single-instance flock on path (one engine per
// user). The lock dies with the process, so a crashed engine never
// blocks the next one.
func lockEngine(path string) (unlock func(), err error) {
	if err := ensurePrivateDir(filepath.Dir(path)); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("engine lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("another backpack engine is already running (%s is locked)", path)
		}
		return nil, fmt.Errorf("engine lock %s: %w", path, err)
	}
	return func() { f.Close() }, nil
}

// listenEngine binds the engine socket, mode 0600 inside a 0700
// directory. An existing socket is removed only when no engine answers on
// it.
func listenEngine(sock string) (net.Listener, error) {
	if err := paths.CheckSocketPath(sock); err != nil {
		return nil, err
	}
	if err := ensurePrivateDir(filepath.Dir(sock)); err != nil {
		return nil, err
	}
	if _, err := os.Lstat(sock); err == nil {
		pctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
		_, err := engineclient.New(sock).Status(pctx)
		cancel()
		switch {
		case err == nil:
			return nil, fmt.Errorf("another backpack engine is serving %s", sock)
		case !errors.Is(err, contracts.ErrEngineNotRunning):
			return nil, fmt.Errorf("%s exists and did not answer cleanly (%v); remove it if no engine is running", sock, err)
		}
		if err := os.Remove(sock); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("removing stale %s: %w", sock, err)
		}
	}
	ln, err := net.Listen("unix", sock)
	if err != nil {
		return nil, fmt.Errorf("engine socket: %w", err)
	}
	if err := os.Chmod(sock, 0o600); err != nil {
		ln.Close()
		return nil, fmt.Errorf("engine socket: %w", err)
	}
	return ln, nil
}

func removeSocket(sock string) {
	if err := os.Remove(sock); err != nil && !errors.Is(err, os.ErrNotExist) {
		fmt.Fprintf(os.Stderr, "backpack: removing %s: %v\n", sock, err)
	}
}

// ensurePrivateDir creates dir (0700) if needed and makes sure it is 0700.
func ensurePrivateDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return os.Chmod(dir, 0o700)
}

// engineLogSink is where the engine's slog output goes: stderr (the engine
// log file for a detached engine, the terminal for `backpack -f
// --no-tui`), except while the TUI owns the terminal, when lines are
// appended to the engine log file instead.
var engineLogSink = &logSink{}

type logSink struct {
	mu   sync.Mutex
	tui  bool
	path string
	f    *os.File
}

var _ io.Writer = (*logSink)(nil)

func (s *logSink) setFile(path string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.path = path
}

func (s *logSink) setTUI(on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tui = on
}

func (s *logSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.tui || s.path == "" {
		return os.Stderr.Write(p)
	}
	if s.f == nil {
		if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
			return len(p), nil // never draw on the dashboard
		}
		f, err := os.OpenFile(s.path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
		if err != nil {
			return len(p), nil
		}
		s.f = f
	}
	return s.f.Write(p)
}
