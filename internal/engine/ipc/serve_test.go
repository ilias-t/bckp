package ipc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ilias-t/bckp/internal/config"
	"github.com/ilias-t/bckp/internal/contracts"
	"github.com/ilias-t/bckp/internal/engine"
	"github.com/ilias-t/bckp/internal/fakes"
	"github.com/ilias-t/bckp/internal/paths"
	"github.com/ilias-t/bckp/internal/proto"
)

// fakeController records calls and answers from its fields.
type fakeController struct {
	mu        sync.Mutex
	snap      contracts.Snapshot
	stops     []*bool
	extends   []string
	stopErr   error
	extendErr error
	aborts    int
	abortErr  error
	stream    []contracts.Snapshot // sent by Subscribe, then closed
	holdOpen  bool                 // keep the stream open until ctx ends
	subCtxEnd chan struct{}
}

func (f *fakeController) Snapshot() contracts.Snapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.snap
}

func (f *fakeController) Stop(_ context.Context, sleep *bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stops = append(f.stops, sleep)
	if f.stopErr == nil {
		f.snap.Session.Ended = true
	}
	return f.stopErr
}

func (f *fakeController) Extend(_ context.Context, minutes int, forever bool) (contracts.Snapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.extends = append(f.extends, fmt.Sprintf("%d/%t", minutes, forever))
	return f.snap, f.extendErr
}

func (f *fakeController) AbortSleep(context.Context) (contracts.Snapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.aborts++
	return f.snap, f.abortErr
}

func (f *fakeController) Subscribe(ctx context.Context) <-chan contracts.Snapshot {
	ch := make(chan contracts.Snapshot)
	go func() {
		defer close(ch)
		for _, s := range f.stream {
			select {
			case ch <- s:
			case <-ctx.Done():
				return
			}
		}
		if f.holdOpen {
			<-ctx.Done()
			if f.subCtxEnd != nil {
				close(f.subCtxEnd)
			}
		}
	}()
	return ch
}

// serve starts Serve on a fresh unix socket under /tmp (t.TempDir() paths
// can exceed the 104-byte sun_path limit on macOS).
func serve(t *testing.T, c Controller) (sock string, stop func() error) {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "bckp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock = filepath.Join(dir, "engine.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, ln, c) }()
	stopped := false
	stop = func() error {
		if stopped {
			return nil
		}
		stopped = true
		cancel()
		select {
		case err := <-done:
			return err
		case <-time.After(5 * time.Second):
			return errors.New("Serve did not return")
		}
	}
	t.Cleanup(func() { _ = stop() })
	return sock, stop
}

func dial(t *testing.T, sock string) net.Conn {
	t.Helper()
	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	return conn
}

func request(t *testing.T, sock, line string) proto.EngineResponse {
	t.Helper()
	conn := dial(t, sock)
	if _, err := io.WriteString(conn, line+"\n"); err != nil {
		t.Fatal(err)
	}
	var resp proto.EngineResponse
	if err := proto.NewLineReader(conn, proto.MaxEngineResponseBytes).Decode(&resp, false); err != nil {
		t.Fatalf("%s: %v", line, err)
	}
	return resp
}

func TestStatusStopExtend(t *testing.T) {
	f := &fakeController{snap: contracts.Snapshot{PID: 7}}
	sock, _ := serve(t, f)

	if r := request(t, sock, `{"op":"status"}`); !r.OK || r.Snapshot == nil || r.Snapshot.PID != 7 {
		t.Fatalf("status = %+v", r)
	}
	if r := request(t, sock, `{"op":"extend","minutes":30}`); !r.OK || r.Snapshot == nil {
		t.Fatalf("extend = %+v", r)
	}
	if r := request(t, sock, `{"op":"extend","forever":true}`); !r.OK {
		t.Fatalf("extend forever = %+v", r)
	}
	for _, line := range []string{`{"op":"stop"}`, `{"op":"stop","sleep":true}`, `{"op":"stop","sleep":false}`} {
		if r := request(t, sock, line); !r.OK || r.Snapshot == nil || !r.Snapshot.Session.Ended {
			t.Fatalf("%s = %+v", line, r)
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if strings.Join(f.extends, ",") != "30/false,0/true" {
		t.Fatalf("extends = %v", f.extends)
	}
	if len(f.stops) != 3 || f.stops[0] != nil || !*f.stops[1] || *f.stops[2] {
		t.Fatalf("stops = %v", f.stops)
	}
}

func TestErrorMapping(t *testing.T) {
	f := &fakeController{
		stopErr:   contracts.ErrNotAbortable,
		extendErr: fmt.Errorf("engine: %w", contracts.ErrNotAbortable),
	}
	sock, _ := serve(t, f)
	for _, line := range []string{`{"op":"stop","sleep":false}`, `{"op":"extend","minutes":5}`} {
		r := request(t, sock, line)
		if r.OK || r.Code != proto.CodeNotAbortable || !errors.Is(r.Err(), contracts.ErrNotAbortable) {
			t.Fatalf("%s = %+v", line, r)
		}
	}
	f.mu.Lock()
	f.stopErr = errors.New("boom")
	f.mu.Unlock()
	if r := request(t, sock, `{"op":"stop"}`); r.OK || r.Code != proto.CodeInternal {
		t.Fatalf("internal = %+v", r)
	}
}

func TestAbortSleep(t *testing.T) {
	f := &fakeController{snap: contracts.Snapshot{PID: 9}}
	sock, _ := serve(t, f)
	if r := request(t, sock, `{"op":"abort_sleep"}`); !r.OK || r.Snapshot == nil || r.Snapshot.PID != 9 {
		t.Fatalf("abort_sleep = %+v", r)
	}
	for err, code := range map[error]proto.ErrorCode{
		contracts.ErrNoSleepPending: proto.CodeNoSleepPending,
		contracts.ErrNotAbortable:   proto.CodeNotAbortable,
	} {
		f.mu.Lock()
		f.abortErr = err
		f.mu.Unlock()
		r := request(t, sock, `{"op":"abort_sleep"}`)
		if r.OK || r.Code != code || !errors.Is(r.Err(), err) {
			t.Fatalf("%v: %+v", err, r)
		}
	}
	if r := request(t, sock, `{"op":"abort_sleep","minutes":1}`); r.OK || r.Code != proto.CodeBadRequest {
		t.Fatalf("abort_sleep with minutes = %+v", r)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.aborts != 3 {
		t.Fatalf("aborts = %d", f.aborts)
	}
}

func TestBadRequests(t *testing.T) {
	sock, _ := serve(t, &fakeController{})
	for line, code := range map[string]proto.ErrorCode{
		`{nope`:          proto.CodeBadRequest,
		`{"op":"dance"}`: proto.CodeBadRequest,
		`{}`:             proto.CodeBadRequest,
		`{"op":"extend","minutes":5,"forever":true}`: proto.CodeBadRequest,
		`{"op":"extend","minutes":0}`:                proto.CodeBadRequest,
		`{"op":"status","sleep":true}`:               proto.CodeBadRequest,
		`{"op":"status","x":1}`:                      "", // lenient: unknown fields are fine
		`{"op":"status","pad":"` + strings.Repeat("a", proto.MaxEngineRequestBytes) + `"}`: proto.CodeTooLarge,
	} {
		r := request(t, sock, line)
		if code == "" {
			if !r.OK {
				t.Errorf("%.40s: %+v", line, r)
			}
			continue
		}
		if r.OK || r.Code != code {
			t.Errorf("%.40s: got %+v, want %s", line, r.Result, code)
		}
	}
}

func TestSubscribeStreamsUntilDone(t *testing.T) {
	f := &fakeController{stream: []contracts.Snapshot{
		{PID: 1}, {PID: 2}, {PID: 3, Session: contracts.SessionStatus{Ended: true, EndReason: contracts.EndStopped}},
	}}
	sock, _ := serve(t, f)
	conn := dial(t, sock)
	io.WriteString(conn, `{"op":"subscribe"}`+"\n")
	lr := proto.NewLineReader(conn, proto.MaxEngineResponseBytes)
	var pids []int
	for {
		var r proto.EngineResponse
		err := lr.Decode(&r, false)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		pids = append(pids, r.Snapshot.PID)
		if r.Done != r.Snapshot.Session.Ended {
			t.Fatalf("done flag mismatch: %+v", r)
		}
	}
	if fmt.Sprint(pids) != "[1 2 3]" {
		t.Fatalf("pids = %v", pids)
	}
}

func TestSubscribeClientDisconnectCancels(t *testing.T) {
	f := &fakeController{stream: []contracts.Snapshot{{PID: 1}}, holdOpen: true, subCtxEnd: make(chan struct{})}
	sock, _ := serve(t, f)
	conn := dial(t, sock)
	io.WriteString(conn, `{"op":"subscribe"}`+"\n")
	var r proto.EngineResponse
	if err := proto.NewLineReader(conn, proto.MaxEngineResponseBytes).Decode(&r, false); err != nil || r.Snapshot.PID != 1 {
		t.Fatalf("first = %+v %v", r, err)
	}
	conn.Close()
	select {
	case <-f.subCtxEnd:
	case <-time.After(5 * time.Second):
		t.Fatal("subscription not cancelled after the client left")
	}
}

func TestServeShutdown(t *testing.T) {
	f := &fakeController{stream: []contracts.Snapshot{{PID: 1}}, holdOpen: true}
	sock, stop := serve(t, f)
	conn := dial(t, sock)
	io.WriteString(conn, `{"op":"subscribe"}`+"\n")
	lr := proto.NewLineReader(conn, proto.MaxEngineResponseBytes)
	var r proto.EngineResponse
	if err := lr.Decode(&r, false); err != nil {
		t.Fatal(err)
	}
	if err := stop(); err != nil {
		t.Fatalf("Serve = %v", err)
	}
	if _, err := lr.ReadLine(); !errors.Is(err, io.EOF) {
		t.Fatalf("stream must close on shutdown, got %v", err)
	}
	if _, err := net.Dial("unix", sock); err == nil {
		t.Fatal("listener must be closed")
	}
}

func TestSilentClientTimesOut(t *testing.T) {
	if testing.Short() {
		t.Skip("waits RequestReadTimeout")
	}
	sock, _ := serve(t, &fakeController{})
	conn := dial(t, sock)
	_ = conn.SetDeadline(time.Now().Add(RequestReadTimeout + 3*time.Second))
	start := time.Now()
	if _, err := conn.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Fatalf("want EOF, got %v", err)
	}
	if d := time.Since(start); d < RequestReadTimeout-time.Second {
		t.Fatalf("closed after %v", d)
	}
}

// gatedStop is a Controller whose Stop signals entered and waits for
// release (or ctx's end), then fails with ctx's error if ctx has ended.
// That is the choice the real engine makes at random when its session has
// ended and ctx is cancelled at once (Engine.Stop selects on both), made
// deterministic.
type gatedStop struct {
	fakeController
	entered, release chan struct{}
}

func newGatedStop() *gatedStop {
	return &gatedStop{entered: make(chan struct{}), release: make(chan struct{})}
}

func (g *gatedStop) Stop(ctx context.Context, sleep *bool) error {
	close(g.entered)
	select {
	case <-g.release:
	case <-ctx.Done():
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return g.fakeController.Stop(ctx, sleep)
}

// A stop whose engine call finishes after shutdown has begun still gets
// its real answer: in the process, the stop request that ends the session
// is in flight when Serve is cancelled.
func TestShutdownAnswersInFlightRequest(t *testing.T) {
	g := newGatedStop()
	sock, stop := serve(t, g)
	conn := dial(t, sock)
	io.WriteString(conn, `{"op":"stop"}`+"\n")
	<-g.entered

	stopped := make(chan error, 1)
	go func() { stopped <- stop() }()
	// Shutdown has begun once the listener is closed.
	deadline := time.Now().Add(5 * time.Second)
	for {
		c, err := net.Dial("unix", sock)
		if err != nil {
			break
		}
		c.Close()
		if time.Now().After(deadline) {
			t.Fatal("listener still open after shutdown")
		}
		time.Sleep(time.Millisecond)
	}
	close(g.release)

	var r proto.EngineResponse
	if err := proto.NewLineReader(conn, proto.MaxEngineResponseBytes).Decode(&r, false); err != nil {
		t.Fatal(err)
	}
	if !r.OK || r.Snapshot == nil || !r.Snapshot.Session.Ended {
		t.Fatalf("stop during shutdown = %+v", r)
	}
	if err := <-stopped; err != nil {
		t.Fatalf("Serve = %v", err)
	}
}

// A request stuck in the engine does not hold Serve open past
// ShutdownGrace: its ctx is cancelled then, and it is answered with that
// error.
func TestShutdownBoundsStuckRequest(t *testing.T) {
	defer func(d time.Duration) { ShutdownGrace = d }(ShutdownGrace)
	ShutdownGrace = 50 * time.Millisecond
	g := newGatedStop() // never released
	sock, stop := serve(t, g)
	conn := dial(t, sock)
	io.WriteString(conn, `{"op":"stop"}`+"\n")
	<-g.entered

	if err := stop(); err != nil {
		t.Fatalf("Serve = %v", err)
	}
	var r proto.EngineResponse
	if err := proto.NewLineReader(conn, proto.MaxEngineResponseBytes).Decode(&r, false); err != nil {
		t.Fatal(err)
	}
	if r.OK || r.Code != proto.CodeInternal {
		t.Fatalf("stuck stop = %+v", r)
	}
}

// TestWithRealEngine drives a real engine (fakes, never-advanced fake
// clock) over the socket: status, subscribe, extend, then stop.
func TestWithRealEngine(t *testing.T) {
	clock := fakes.NewClock(time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC))
	guard := fakes.NewSleepGuard(clock)
	nw := fakes.NewNetwork(clock, "Wi-Fi", "iPhone USB")
	nw.PushSnapshot(contracts.NetSnapshot{DefaultIface: "en10", Online: true, IfacePresent: true})
	idle := make(chan struct{}, 1)
	e := engine.New(engine.Deps{
		Clock:    clock,
		Thermal:  fakes.NewThermal(clock, contracts.ThermalReading{State: contracts.ThermalNominal}),
		Power:    fakes.NewPower(clock, contracts.PowerReading{OnAC: true, Percent: 100}),
		Network:  nw,
		Notifier: fakes.NewNotifier(),
		Guard:    guard,
		Config:   config.Default(),
		Paths:    paths.ForHome(t.TempDir()),
		LoadAvg:  func() (float64, error) { return 1, nil },
		OnIdle: func(time.Time) {
			select {
			case idle <- struct{}{}:
			default:
			}
		},
	})
	runErr := make(chan error, 1)
	go func() { runErr <- e.Run(context.Background(), engine.Options{Duration: time.Hour}) }()
	select {
	case <-idle:
	case <-time.After(5 * time.Second):
		t.Fatal("engine did not start")
	}
	sock, stop := serve(t, e)

	r := request(t, sock, `{"op":"status"}`)
	if !r.OK || !r.Snapshot.Lease.Held || r.Snapshot.Session.RemainingS != 3600 {
		t.Fatalf("status = %+v", r)
	}

	sub := dial(t, sock)
	io.WriteString(sub, `{"op":"subscribe"}`+"\n")
	lr := proto.NewLineReader(sub, proto.MaxEngineResponseBytes)
	var first proto.EngineResponse
	if err := lr.Decode(&first, false); err != nil || first.Done {
		t.Fatalf("first = %+v %v", first, err)
	}

	r = request(t, sock, `{"op":"extend","minutes":30}`)
	if !r.OK || r.Snapshot.Session.RemainingS != 5400 {
		t.Fatalf("extend = %+v", r)
	}

	r = request(t, sock, `{"op":"stop","sleep":false}`)
	if !r.OK || !r.Snapshot.Session.Ended || r.Snapshot.Session.EndReason != contracts.EndStopped {
		t.Fatalf("stop = %+v", r)
	}
	var last proto.EngineResponse
	for {
		var resp proto.EngineResponse
		err := lr.Decode(&resp, false)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		last = resp
	}
	if !last.Done || !last.Snapshot.Session.Ended {
		t.Fatalf("subscribe must end with done: %+v", last)
	}
	if err := <-runErr; err != nil {
		t.Fatal(err)
	}
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	if guard.Leases()[0].Releases() != 1 {
		t.Fatal("lease not released")
	}
}
