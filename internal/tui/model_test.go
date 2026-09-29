package tui

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/muesli/termenv"

	"github.com/ilias-t/bckp/internal/contracts"
	"github.com/ilias-t/bckp/internal/fakes"
	"github.com/ilias-t/bckp/internal/proto"
)

// runCmd runs cmd with a timeout and returns its message.
func runCmd(t *testing.T, cmd tea.Cmd) tea.Msg {
	t.Helper()
	if cmd == nil {
		t.Fatal("expected a command, got nil")
	}
	done := make(chan tea.Msg, 1)
	go func() { done <- cmd() }()
	select {
	case msg := <-done:
		return msg
	case <-time.After(2 * time.Second):
		t.Fatal("command did not finish")
		return nil
	}
}

func step(t *testing.T, m model, msg tea.Msg) (model, tea.Cmd) {
	t.Helper()
	next, cmd := m.Update(msg)
	return next.(model), cmd
}

func key(s string) tea.KeyMsg {
	switch s {
	case "ctrl+c":
		return tea.KeyMsg{Type: tea.KeyCtrlC}
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
}

// liveModel subscribes to c and applies the first snapshot.
func liveModel(t *testing.T, c contracts.EngineClient) model {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	subCtx, subCancel := context.WithCancel(ctx)
	ch, err := c.Subscribe(subCtx)
	if err != nil {
		t.Fatal(err)
	}
	m := newModel(ctx, c, ch, subCancel, newStyles(termenv.Ascii))
	m.loc = time.UTC
	m.clock = func() time.Time { return base }
	m.backoff = func(int) time.Duration { return time.Millisecond }
	m, cmd := step(t, m, runCmd(t, waitCmd(ch, m.gen)))
	if m.conn != connLive || m.snap == nil {
		t.Fatalf("model not live after first snapshot: conn=%v", m.conn)
	}
	if cmd == nil {
		t.Fatal("no wait command after first snapshot")
	}
	return m
}

// press sends a key and, when it yields a command, runs it and applies
// the resulting message (one round).
func press(t *testing.T, m model, k string) model {
	t.Helper()
	m, cmd := step(t, m, key(k))
	if cmd == nil {
		return m
	}
	msg := runCmd(t, cmd)
	if _, ok := msg.(tea.QuitMsg); ok {
		return m
	}
	m, _ = step(t, m, msg)
	return m
}

func TestKeyExtendAndForever(t *testing.T) {
	f := fakes.NewEngineClient(nominalSnap())
	m := liveModel(t, f)

	m = press(t, m, "e")
	if !strings.Contains(m.flash, "extended by 30m") || m.flashErr {
		t.Errorf("flash after e = %q (err=%v)", m.flash, m.flashErr)
	}
	m = press(t, m, "f")
	want := []fakes.ExtendCall{{Minutes: 30}, {Forever: true}}
	if got := f.Extends(); !equalExtends(got, want) {
		t.Errorf("Extends = %+v, want %+v", got, want)
	}
	if len(f.Stops()) != 0 {
		t.Errorf("unexpected Stop calls: %v", f.Stops())
	}
}

func TestKeyAbortSleep(t *testing.T) {
	t.Run("abortable", func(t *testing.T) {
		f := fakes.NewEngineClient(seriousSnap())
		m := liveModel(t, f)
		m = press(t, m, "x")
		if f.Aborts() != 1 || len(f.Extends()) != 0 {
			t.Errorf("x must send abort_sleep, not extend: aborts %d, extends %+v", f.Aborts(), f.Extends())
		}
		if m.flash != "pending sleep aborted" {
			t.Errorf("flash = %q", m.flash)
		}
		if m.snap.SleepPending != nil {
			t.Errorf("snapshot from abort not applied: %+v", m.snap.SleepPending)
		}
	})
	t.Run("nothing pending", func(t *testing.T) {
		f := fakes.NewEngineClient(nominalSnap())
		m := liveModel(t, f)
		m = press(t, m, "x")
		if f.Aborts() != 0 {
			t.Errorf("abort sent with no pending sleep")
		}
		if m.flash != "no sleep pending" || !m.flashErr {
			t.Errorf("flash = %q (err=%v)", m.flash, m.flashErr)
		}
	})
	t.Run("critical is not sent", func(t *testing.T) {
		s := seriousSnap()
		s.SleepPending.Trigger, s.SleepPending.Abortable = contracts.SleepTriggerCritical, false
		f := fakes.NewEngineClient(s)
		m := liveModel(t, f)
		m = press(t, m, "x")
		if f.Aborts() != 0 {
			t.Errorf("abort sent for a critical sleep")
		}
		if m.flash != notAbortableText || !m.flashErr {
			t.Errorf("flash = %q (err=%v)", m.flash, m.flashErr)
		}
		if out := render(m); !strings.Contains(out, notAbortableText) {
			t.Errorf("not_abortable not shown inline:\n%s", out)
		}
	})
	t.Run("engine answers not_abortable", func(t *testing.T) {
		// The snapshot is stale (still abortable) but the engine has
		// escalated to critical: the not_abortable error is shown inline.
		f := fakes.NewEngineClient(seriousSnap())
		notAbortable := &proto.RemoteError{Code: proto.CodeNotAbortable, Message: "critical sleep pending"}
		f.SetErrs(nil, nil, notAbortable)
		f.SetAbortErr(notAbortable)
		m := liveModel(t, f)
		for _, k := range []string{"x", "e", "f"} {
			m.flash = ""
			m = press(t, m, k)
			if m.flash != notAbortableText || !m.flashErr {
				t.Errorf("%s: flash = %q (err=%v)", k, m.flash, m.flashErr)
			}
		}
		if len(f.Extends()) != 2 || f.Aborts() != 1 {
			t.Errorf("Extends = %+v, aborts = %d", f.Extends(), f.Aborts())
		}
	})
	t.Run("engine answers no_sleep_pending", func(t *testing.T) {
		// The sleep ended (or was aborted elsewhere) after the snapshot.
		f := fakes.NewEngineClient(seriousSnap())
		f.SetAbortErr(&proto.RemoteError{Code: proto.CodeNoSleepPending, Message: "no sleep pending"})
		m := liveModel(t, f)
		m = press(t, m, "x")
		if m.flash != "no sleep pending" || !m.flashErr {
			t.Errorf("flash = %q (err=%v)", m.flash, m.flashErr)
		}
	})
}

func TestKeyStopConfirm(t *testing.T) {
	for _, tc := range []struct {
		name      string
		keys      []string
		wantStops int
	}{
		{"s then y", []string{"s", "y"}, 1},
		{"s then enter", []string{"s", "enter"}, 1},
		{"s then n cancels", []string{"s", "n"}, 0},
		{"s then q cancels, does not detach", []string{"s", "q"}, 0},
		{"ctrl+c then y", []string{"ctrl+c", "y"}, 1},
		{"ctrl+c twice", []string{"ctrl+c", "ctrl+c"}, 1},
		{"ctrl+c then esc cancels", []string{"ctrl+c", "esc"}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := fakes.NewEngineClient(nominalSnap())
			m := liveModel(t, f)
			m = press(t, m, tc.keys[0])
			if !m.confirmStop {
				t.Fatalf("%q did not ask for confirmation", tc.keys[0])
			}
			if out := render(m); !strings.Contains(out, "Stop the session? [y/N]") {
				t.Errorf("confirm prompt not rendered:\n%s", out)
			}
			if len(f.Stops()) != 0 {
				t.Fatal("Stop called before confirmation")
			}
			m = press(t, m, tc.keys[1])
			if m.confirmStop {
				t.Error("still confirming")
			}
			if m.exit != exitNone {
				t.Errorf("exit = %v, want none", m.exit)
			}
			stops := f.Stops()
			if len(stops) != tc.wantStops {
				t.Fatalf("Stops = %v, want %d", stops, tc.wantStops)
			}
			if tc.wantStops == 1 && stops[0] != nil {
				t.Errorf("Stop sleep = %v, want nil", *stops[0])
			}
		})
	}
}

func TestKeyStopError(t *testing.T) {
	f := fakes.NewEngineClient(nominalSnap())
	f.SetErrs(nil, errors.New("boom"), nil)
	m := liveModel(t, f)
	m = press(t, m, "s")
	m = press(t, m, "y")
	if !m.flashErr || !strings.Contains(m.flash, "stop failed: boom") {
		t.Errorf("flash = %q (err=%v)", m.flash, m.flashErr)
	}
}

func TestKeyDetach(t *testing.T) {
	f := fakes.NewEngineClient(nominalSnap())
	m := liveModel(t, f)
	m, cmd := step(t, m, key("q"))
	if _, ok := runCmd(t, cmd).(tea.QuitMsg); !ok {
		t.Fatal("q did not quit")
	}
	if m.exit != exitDetached || exitLine(m) != DetachedMessage {
		t.Errorf("exit = %v, line %q", m.exit, exitLine(m))
	}
	if len(f.Stops())+len(f.Extends()) != 0 {
		t.Error("detach must not call the engine")
	}
}

func TestForegroundQuitAsksToStop(t *testing.T) {
	f := fakes.NewEngineClient(nominalSnap())
	m := liveModel(t, f)
	m.foreground = true

	m, cmd := step(t, m, key("q"))
	if cmd != nil || m.exit != exitNone || !m.confirmStop || !m.confirmQuit {
		t.Fatalf("q in a foreground session must ask, not quit: exit %v confirm %v", m.exit, m.confirmStop)
	}
	if out := render(m); !strings.Contains(out, ForegroundQuitText) {
		t.Errorf("foreground confirm not shown:\n%s", out)
	}
	// Anything but yes keeps the session running.
	m = press(t, m, "n")
	if m.confirmStop || len(f.Stops()) != 0 || m.flash != "stop cancelled" {
		t.Fatalf("n: confirm %v stops %v flash %q", m.confirmStop, f.Stops(), m.flash)
	}
	if out := render(m); !strings.Contains(out, "q stop (-f)") {
		t.Errorf("foreground key hint not shown:\n%s", out)
	}
	// q, y stops.
	m = press(t, m, "q")
	m = press(t, m, "y")
	if got := f.Stops(); len(got) != 1 || got[0] != nil {
		t.Fatalf("stops = %v", got)
	}
	if m.exit == exitDetached || exitLine(m) == DetachedMessage {
		t.Fatal("a foreground session must never claim to be detached")
	}
}

func TestHelpToggle(t *testing.T) {
	f := fakes.NewEngineClient(nominalSnap())
	m := liveModel(t, f)
	m = press(t, m, "?")
	if !m.showHelp || !strings.Contains(render(m), "Keys") {
		t.Fatal("help not shown")
	}
	m = press(t, m, "e") // any key closes help without acting
	if m.showHelp || len(f.Extends()) != 0 {
		t.Errorf("help still shown=%v or key acted: %+v", m.showHelp, f.Extends())
	}
}

func TestEndedSession(t *testing.T) {
	f := fakes.NewEngineClient(nominalSnap())
	m := liveModel(t, f)
	next := waitCmd(m.sub, m.gen)
	f.End(contracts.EndStopped)
	m, cmd := step(t, m, runCmd(t, next))
	if m.conn != connEnded || cmd != nil {
		t.Fatalf("conn = %v, cmd = %v; want ended with no command", m.conn, cmd)
	}
	out := render(m)
	if !strings.Contains(out, "Session ended: stopped") || !strings.Contains(out, "press any key to exit") {
		t.Errorf("ended screen:\n%s", out)
	}
	m, cmd = step(t, m, key("e"))
	if _, ok := runCmd(t, cmd).(tea.QuitMsg); !ok || m.exit != exitEnded {
		t.Fatalf("keypress after end: exit=%v", m.exit)
	}
	if len(f.Extends()) != 0 {
		t.Error("keypress after end must not call the engine")
	}
	if got := exitLine(m); got != "session ended: stopped" {
		t.Errorf("exit line = %q", got)
	}
}

func TestHistoryDedupAndCap(t *testing.T) {
	m := testModel(nil, 80, termenv.Ascii, nil)
	s := nominalSnap()
	m.applySnapshot(s)
	m.applySnapshot(s) // same reading re-sent on a state change
	if len(m.history) != 1 {
		t.Fatalf("history = %v, want 1 sample", m.history)
	}
	for i := range 2 * historyLen {
		s.Thermal.Reading = &contracts.ThermalReading{State: contracts.ThermalNominal, CPUC: fp(float64(i)), At: base.Add(time.Duration(i+1) * 15 * time.Second)}
		m.applySnapshot(s)
	}
	if len(m.history) != historyLen || m.history[historyLen-1] != float64(2*historyLen-1) {
		t.Errorf("history len %d, last %v", len(m.history), m.history[len(m.history)-1])
	}
	s.Thermal.Reading = &contracts.ThermalReading{State: contracts.ThermalNominal, At: base.Add(time.Hour)}
	m.applySnapshot(s)
	if len(m.history) != historyLen {
		t.Error("a reading without CPU °C must not add a sample")
	}
}

// switchClient fronts a sequence of fake engines, as if the engine
// restarted: calls go to the first fake that is still running. failSubs
// makes the next N Subscribe calls fail.
type switchClient struct {
	mu       sync.Mutex
	engines  []*fakes.EngineClient
	failSubs int
	subCalls int
}

func (s *switchClient) cur() *fakes.EngineClient {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.engines {
		if _, err := e.Status(context.Background()); err == nil {
			return e
		}
	}
	return s.engines[len(s.engines)-1]
}

func (s *switchClient) Status(ctx context.Context) (contracts.Snapshot, error) {
	return s.cur().Status(ctx)
}

func (s *switchClient) Stop(ctx context.Context, sleep *bool) error { return s.cur().Stop(ctx, sleep) }

func (s *switchClient) Extend(ctx context.Context, minutes int, forever bool) (contracts.Snapshot, error) {
	return s.cur().Extend(ctx, minutes, forever)
}

func (s *switchClient) AbortSleep(ctx context.Context) (contracts.Snapshot, error) {
	return s.cur().AbortSleep(ctx)
}

func (s *switchClient) Subscribe(ctx context.Context) (<-chan contracts.Snapshot, error) {
	s.mu.Lock()
	s.subCalls++
	fail := s.failSubs > 0
	if fail {
		s.failSubs--
	}
	s.mu.Unlock()
	if fail {
		return nil, contracts.ErrEngineNotRunning
	}
	return s.cur().Subscribe(ctx)
}

func (s *switchClient) calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.subCalls
}

func TestReconnectOnDrop(t *testing.T) {
	first := fakes.NewEngineClient(nominalSnap())
	restarted := nominalSnap()
	restarted.PID = 5151
	second := fakes.NewEngineClient(restarted)
	c := &switchClient{engines: []*fakes.EngineClient{first, second}}
	m := liveModel(t, c)

	// The connection drops without an Ended snapshot.
	next := waitCmd(m.sub, m.gen)
	c.mu.Lock()
	c.failSubs = 1 // the first retry fails
	c.mu.Unlock()
	first.Drop()
	m, retry := step(t, m, runCmd(t, next))
	if m.conn != connLost || m.attempt != 1 {
		t.Fatalf("after drop: conn=%v attempt=%d", m.conn, m.attempt)
	}
	if out := render(m); !strings.Contains(out, "connection lost") {
		t.Errorf("banner missing:\n%s", out)
	}
	// Actions are refused while disconnected.
	m = press(t, m, "e")
	if !m.flashErr || len(second.Extends()) != 0 {
		t.Errorf("extend while lost: flash=%q extends=%v", m.flash, second.Extends())
	}

	// Retry 1 fails; a second retry is scheduled.
	m, sub := step(t, m, runCmd(t, retry))
	m, retry = step(t, m, runCmd(t, sub))
	if m.conn != connLost || m.attempt != 2 || m.lastErr != "engine not running" {
		t.Fatalf("after failed retry: conn=%v attempt=%d err=%q", m.conn, m.attempt, m.lastErr)
	}

	// Retry 2 reaches the restarted engine.
	m, sub = step(t, m, runCmd(t, retry))
	m, wait := step(t, m, runCmd(t, sub))
	m, _ = step(t, m, runCmd(t, wait))
	if m.conn != connLive || m.attempt != 0 || m.snap.PID != 5151 {
		t.Fatalf("after reconnect: conn=%v attempt=%d pid=%d err=%q", m.conn, m.attempt, m.snap.PID, m.lastErr)
	}
	if c.calls() != 3 || second.Subscribers() != 1 {
		t.Errorf("subscribe calls = %d, second subscribers = %d", c.calls(), second.Subscribers())
	}
	if out := render(m); strings.Contains(out, "connection lost") {
		t.Errorf("banner still shown after reconnect:\n%s", out)
	}
	// Stale messages from the dropped subscription are ignored.
	if m2, cmd := step(t, m, subClosedMsg{gen: 0}); m2.conn != connLive || cmd != nil {
		t.Error("stale close from generation 0 was not ignored")
	}
}

func TestDefaultBackoff(t *testing.T) {
	want := []time.Duration{500 * time.Millisecond, time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 10 * time.Second, 10 * time.Second}
	for i, w := range want {
		if got := defaultBackoff(i); got != w {
			t.Errorf("defaultBackoff(%d) = %v, want %v", i, got, w)
		}
	}
}

// TestRunReconnectsAndDetaches drives the real bubbletea program: the
// engine drops, the TUI resubscribes to the restarted engine, and q detaches.
func TestRunReconnectsAndDetaches(t *testing.T) {
	first := fakes.NewEngineClient(nominalSnap())
	second := fakes.NewEngineClient(nominalSnap())
	c := &switchClient{engines: []*fakes.EngineClient{first, second}}

	inR, inW := io.Pipe()
	defer inW.Close()
	out := &syncBuffer{}
	done := make(chan error, 1)
	go func() { done <- run(context.Background(), c, inR, out, Options{}) }()

	waitFor(t, "first subscription", func() bool { return first.Subscribers() == 1 })
	first.Drop()
	waitFor(t, "resubscribe after drop", func() bool { return second.Subscribers() == 1 })

	if _, err := inW.Write([]byte("q")); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not return after q")
	}
	if !strings.Contains(out.String(), DetachedMessage) {
		t.Errorf("detach message missing from output")
	}
	if len(second.Stops()) != 0 {
		t.Error("detach stopped the engine")
	}
	waitFor(t, "unsubscribe on exit", func() bool { return second.Subscribers() == 0 })
}

func TestRunNotRunning(t *testing.T) {
	f := fakes.NewEngineClient(nominalSnap())
	f.Drop()
	err := run(context.Background(), f, strings.NewReader(""), io.Discard, Options{})
	if !errors.Is(err, contracts.ErrEngineNotRunning) {
		t.Fatalf("err = %v, want ErrEngineNotRunning", err)
	}
}

// TestRunContextCancel: an external SIGINT during `attach` reaches the TUI
// as a cancelled ctx. It must detach cleanly (nil, "still running"), not
// fail with "context canceled"; in the foreground it prints nothing, since
// the caller is stopping the engine.
func TestRunContextCancel(t *testing.T) {
	for _, fg := range []bool{false, true} {
		f := fakes.NewEngineClient(nominalSnap())
		ctx, cancel := context.WithCancel(context.Background())
		inR, inW := io.Pipe()
		out := &syncBuffer{}
		done := make(chan error, 1)
		go func() { done <- run(ctx, f, inR, out, Options{Foreground: fg}) }()
		waitFor(t, "subscription", func() bool { return f.Subscribers() == 1 })
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("foreground=%v: err = %v, want nil", fg, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("run did not return after cancel")
		}
		inW.Close()
		if got := strings.Contains(out.String(), DetachedMessage); got == fg {
			t.Errorf("foreground=%v: detach message printed = %v", fg, got)
		}
		if len(f.Stops()) != 0 {
			t.Error("cancel stopped the engine")
		}
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func equalExtends(a, b []fakes.ExtendCall) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
