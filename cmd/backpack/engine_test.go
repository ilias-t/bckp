package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/ilias-t/bckp/internal/config"
	"github.com/ilias-t/bckp/internal/contracts"
	"github.com/ilias-t/bckp/internal/engine"
	"github.com/ilias-t/bckp/internal/engine/ipc"
	"github.com/ilias-t/bckp/internal/engineclient"
	"github.com/ilias-t/bckp/internal/fakes"
	"github.com/ilias-t/bckp/internal/paths"
	"github.com/ilias-t/bckp/internal/system"
)

// shortHome returns a temp home short enough for the engine socket path
// (t.TempDir() on macOS can exceed paths.MaxSocketPathLen).
func shortHome(t *testing.T) paths.User {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "bckp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return paths.ForHome(dir)
}

type session struct {
	u     paths.User
	e     *engine.Engine
	guard *fakes.SleepGuard
	note  *fakes.Notifier
	sigs  chan os.Signal
	done  chan error
}

// startSession runs serveSession with fake providers (network off) until
// the returned session's done channel yields Run's result.
func startSession(t *testing.T, ctx context.Context) *session {
	t.Helper()
	u := shortHome(t)
	cfg := config.Default()
	cfg.Network.Watchdog = false
	cfg.Network.PreferHotspot = config.HotspotOff
	s := &session{
		u:     u,
		guard: fakes.NewSleepGuard(nil),
		note:  fakes.NewNotifier(),
		sigs:  make(chan os.Signal, 1),
		done:  make(chan error, 1),
	}
	s.e = engine.New(engine.Deps{
		Clock:    system.Clock{},
		Thermal:  fakes.NewThermal(nil, contracts.ThermalReading{State: contracts.ThermalNominal}),
		Power:    fakes.NewPower(nil, contracts.PowerReading{OnAC: true, Percent: 100}),
		Notifier: s.note,
		Guard:    s.guard,
		Config:   cfg,
		Paths:    u,
		LoadAvg:  func() (float64, error) { return 1, nil },
	})
	log := slog.New(slog.DiscardHandler)
	go func() { s.done <- serveSession(ctx, s.sigs, s.e, engine.Options{}, u, log) }()
	s.waitHeld(t)
	return s
}

func (s *session) waitHeld(t *testing.T) {
	t.Helper()
	c := engineclient.New(s.u.EngineSocket)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		snap, err := c.Status(ctx)
		cancel()
		if err == nil && snap.Lease.Held {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("engine never reported a held lease over its socket")
}

func (s *session) wait(t *testing.T) error {
	t.Helper()
	select {
	case err := <-s.done:
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("serveSession did not return")
		return nil
	}
}

// checkEnded asserts a clean end: EndStopped, lease released, no sleep,
// socket removed.
func (s *session) checkEnded(t *testing.T) {
	t.Helper()
	s.checkEndedWith(t, contracts.EndStopped)
}

func (s *session) checkEndedWith(t *testing.T, reason contracts.EndReason) {
	t.Helper()
	snap := s.e.Snapshot()
	if !snap.Session.Ended || snap.Session.EndReason != reason {
		t.Fatalf("session = %+v, want ended with %q", snap.Session, reason)
	}
	leases := s.guard.Leases()
	if len(leases) != 1 {
		t.Fatalf("leases = %d, want 1", len(leases))
	}
	if leases[0].Releases() != 1 || s.guard.Slept() {
		t.Fatalf("releases = %d, slept = %v", leases[0].Releases(), s.guard.Slept())
	}
	if _, err := os.Lstat(s.u.EngineSocket); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("socket left behind: %v", err)
	}
}

// A cancelled ctx without a SIGTERM/SIGHUP (the CLI's SIGINT, the TUI
// exiting) ends the session as a stop, not as EndSignal.
func TestCancelStopsSessionAsStopped(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	s := startSession(t, ctx)
	fi, err := os.Lstat(s.u.EngineSocket)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("socket: %v, mode %v", err, fi.Mode())
	}
	if di, err := os.Stat(filepath.Dir(s.u.EngineSocket)); err != nil || di.Mode().Perm() != 0o700 {
		t.Fatalf("socket dir: %v, mode %v", err, di.Mode())
	}
	cancel()
	if err := s.wait(t); err != nil {
		t.Fatalf("serveSession: %v", err)
	}
	s.checkEnded(t)
}

// SIGTERM or SIGHUP (logout, kill, a closed terminal) ends the session as
// EndSignal, whose notification needs attention, even when the CLI's own
// handler cancels ctx first.
func TestTermSignalEndsSessionAsSignal(t *testing.T) {
	for _, sig := range []os.Signal{syscall.SIGTERM, syscall.SIGHUP} {
		t.Run(sig.String(), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			s := startSession(t, ctx)
			cancel()
			s.sigs <- sig
			if err := s.wait(t); err != nil {
				t.Fatalf("serveSession: %v", err)
			}
			s.checkEndedWith(t, contracts.EndSignal)
			sent := s.note.Sent()
			last := sent[len(sent)-1]
			if last.Class != contracts.ClassAttention || !strings.Contains(last.Body, signalName(sig)+" received") {
				t.Fatalf("last notification = %+v", last)
			}
		})
	}
}

// `backpack stop` over the socket gets its answer, although Serve is
// cancelled as soon as Run returns, possibly before the stop handler has
// seen the session end (ipc.TestShutdownAnswersInFlightRequest forces
// that ordering).
func TestStopRequestIsAnswered(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := startSession(t, ctx)
	sctx, scancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer scancel()
	if err := engineclient.New(s.u.EngineSocket).Stop(sctx, nil); err != nil {
		t.Fatalf("stop over the socket: %v", err)
	}
	if err := s.wait(t); err != nil {
		t.Fatalf("serveSession: %v", err)
	}
	s.checkEnded(t)
}

// The foreground TUI stops the session itself, then the CLI cancels ctx:
// the second stop is a no-op.
func TestStopThenCancelIsSingleStop(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	s := startSession(t, ctx)
	sctx, scancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer scancel()
	if err := engineclient.New(s.u.EngineSocket).Stop(sctx, nil); err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := s.wait(t); err != nil {
		t.Fatalf("serveSession: %v", err)
	}
	s.checkEnded(t)
}

func TestSecondEngineIsRefused(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	s := startSession(t, ctx)
	defer func() { cancel(); s.wait(t) }()
	e2 := engine.New(engine.Deps{})
	err := serveSession(context.Background(), nil, e2, engine.Options{}, s.u, slog.New(slog.DiscardHandler))
	if err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("second engine: %v", err)
	}
	// The first engine's socket is untouched.
	if _, err := engineclient.New(s.u.EngineSocket).Status(context.Background()); err != nil {
		t.Fatalf("first engine unreachable: %v", err)
	}
}

func TestListenEngineRemovesStaleSocket(t *testing.T) {
	u := shortHome(t)
	if err := ensurePrivateDir(filepath.Dir(u.EngineSocket)); err != nil {
		t.Fatal(err)
	}
	// A socket file nobody listens on, as left by a kill -9.
	old, err := net.ListenUnix("unix", &net.UnixAddr{Name: u.EngineSocket, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	old.SetUnlinkOnClose(false)
	old.Close()
	if _, err := os.Lstat(u.EngineSocket); err != nil {
		t.Fatalf("setup: %v", err)
	}
	ln, err := listenEngine(u.EngineSocket)
	if err != nil {
		t.Fatalf("stale socket not replaced: %v", err)
	}
	ln.Close()
}

func TestListenEngineKeepsAnsweringSocket(t *testing.T) {
	u := shortHome(t)
	if err := ensurePrivateDir(filepath.Dir(u.EngineSocket)); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", u.EngineSocket)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- ipc.Serve(ctx, ln, fakeController{}) }()
	defer func() { cancel(); <-served }()

	if _, err := listenEngine(u.EngineSocket); err == nil || !strings.Contains(err.Error(), "another backpack engine") {
		t.Fatalf("listenEngine over a live engine: %v", err)
	}
	if _, err := os.Lstat(u.EngineSocket); err != nil {
		t.Fatalf("live socket removed: %v", err)
	}
}

type fakeController struct{}

func (fakeController) Snapshot() contracts.Snapshot                        { return contracts.Snapshot{PID: 1} }
func (fakeController) Stop(context.Context, *bool) error                   { return nil }
func (fakeController) Subscribe(context.Context) <-chan contracts.Snapshot { return nil }
func (fakeController) Extend(context.Context, int, bool) (contracts.Snapshot, error) {
	return contracts.Snapshot{}, nil
}

func TestNotifierChainOutboxWrapsOnlyNtfy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusBadGateway)
	}))
	defer srv.Close()
	cfg := config.Default()
	cfg.Notify.NtfyServer = srv.URL
	cfg.Notify.NtfyTopic = "test-topic"
	cfg.Notify.MacOS = false // never post real banners from a test
	n := contracts.Notification{Priority: contracts.PriorityDefault, Title: "t", Body: "b"}

	// Session chain: ntfy fails, the message is queued for retry.
	dir := t.TempDir()
	chain := notifierChain(cfg, filepath.Join(dir, "outbox"))
	ob := chain.(contracts.Outbox)
	err := chain.Send(context.Background(), n)
	if !errors.Is(err, contracts.ErrQueued) || ob.Pending() != 1 {
		t.Fatalf("send = %v, pending = %d", err, ob.Pending())
	}

	// Without an outbox dir: the failure is reported, nothing is queued.
	direct := notifierChain(cfg, "")
	err = direct.Send(context.Background(), n)
	if err == nil || errors.Is(err, contracts.ErrQueued) {
		t.Fatalf("direct send = %v", err)
	}

	// No topic and no banners: nothing to send to, the message is accepted.
	cfg.Notify.NtfyTopic = ""
	local := notifierChain(cfg, filepath.Join(dir, "outbox2"))
	if err := local.Send(context.Background(), n); err != nil || local.(contracts.Outbox).Pending() != 0 {
		t.Fatalf("local send: %v", err)
	}
}

// TestChainIsOutbox: the session chain is the engine's contracts.Outbox,
// so a queued "back online" goes out on a later poll.
func TestChainIsOutbox(t *testing.T) {
	var up atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !up.Load() {
			http.Error(w, "down", http.StatusBadGateway)
		}
	}))
	defer srv.Close()
	cfg := config.Default()
	cfg.Notify.NtfyServer = srv.URL
	cfg.Notify.NtfyTopic = "test-topic"
	cfg.Notify.MacOS = false // never post real banners from a test
	chain := notifierChain(cfg, filepath.Join(t.TempDir(), "outbox"))
	ob, ok := chain.(contracts.Outbox)
	if !ok {
		t.Fatal("the session chain is not a contracts.Outbox")
	}
	n := contracts.Notification{Priority: contracts.PriorityDefault, Title: "Mac back online after 15s", Body: "b"}
	if err := chain.Send(context.Background(), n); !errors.Is(err, contracts.ErrQueued) || ob.Pending() != 1 {
		t.Fatalf("send = %v, pending = %d", err, ob.Pending())
	}
	if err := ob.Flush(context.Background()); err == nil || ob.Pending() != 1 {
		t.Fatalf("flush while down = %v, pending = %d", err, ob.Pending())
	}
	up.Store(true)
	if err := ob.Flush(context.Background()); err != nil || ob.Pending() != 0 {
		t.Fatalf("flush = %v, pending = %d", err, ob.Pending())
	}
}

func TestLogSinkAvoidsTerminalWhileTUIRuns(t *testing.T) {
	u := shortHome(t)
	s := &logSink{}
	s.setFile(u.EngineLog)
	s.setTUI(true)
	if _, err := s.Write([]byte("line while tui\n")); err != nil {
		t.Fatal(err)
	}
	s.setTUI(false)
	data, err := os.ReadFile(u.EngineLog)
	if err != nil || string(data) != "line while tui\n" {
		t.Fatalf("engine log = %q, %v", data, err)
	}
}

func (fakeController) AbortSleep(context.Context) (contracts.Snapshot, error) {
	return contracts.Snapshot{}, nil
}

// `notify test` backends: one per configured backend, no outbox, and the
// log backend writes to the logger it is given (never the terminal).
func TestNamedNotifiers(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusBadGateway)
	}))
	defer srv.Close()
	cfg := config.Default()
	cfg.Notify.NtfyServer = srv.URL
	cfg.Notify.NtfyTopic = "test-topic"
	cfg.Notify.MacOS = true
	var buf strings.Builder
	log := slog.New(slog.NewJSONHandler(&buf, nil))
	got := namedNotifiers(cfg, log)
	var names []string
	for _, n := range got {
		names = append(names, n.Name)
	}
	if strings.Join(names, ",") != "ntfy,macOS,log" {
		t.Fatalf("names = %v", names)
	}
	n := contracts.Notification{Priority: contracts.PriorityDefault, Title: "t", Body: "b"}
	if err := got[0].Send(context.Background(), n); err == nil || errors.Is(err, contracts.ErrQueued) {
		t.Fatalf("ntfy send = %v, want the real failure", err)
	}
	if err := got[2].Send(context.Background(), n); err != nil || !strings.Contains(buf.String(), `"title":"t"`) {
		t.Fatalf("log send = %v, log %q", err, buf.String())
	}
	cfg.Notify.NtfyTopic, cfg.Notify.MacOS = "", false
	if got := namedNotifiers(cfg, log); len(got) != 1 || got[0].Name != "log" {
		t.Fatalf("no topic: %v", got)
	}
}
