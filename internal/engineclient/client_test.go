package engineclient

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/ilias-t/bckp/internal/contracts"
	"github.com/ilias-t/bckp/internal/proto"
)

// fakeServer is a tiny engine: one request per connection, answered by
// handle. handle writes zero or more lines.
type fakeServer struct {
	t      *testing.T
	ln     net.Listener
	path   string
	mu     sync.Mutex
	reqs   []proto.EngineRequest
	handle func(req proto.EngineRequest, conn net.Conn)
	wg     sync.WaitGroup
}

func shortDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "bckp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func newFakeServer(t *testing.T, handle func(proto.EngineRequest, net.Conn)) *fakeServer {
	t.Helper()
	path := filepath.Join(shortDir(t), "engine.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	s := &fakeServer{t: t, ln: ln, path: path, handle: handle}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			s.wg.Add(1)
			go func() {
				defer s.wg.Done()
				defer conn.Close()
				var req proto.EngineRequest
				if err := proto.NewLineReader(conn, proto.MaxEngineRequestBytes).Decode(&req, false); err != nil {
					return
				}
				s.mu.Lock()
				s.reqs = append(s.reqs, req)
				s.mu.Unlock()
				s.handle(req, conn)
			}()
		}
	}()
	t.Cleanup(func() { ln.Close(); s.wg.Wait() })
	return s
}

func (s *fakeServer) requests() []proto.EngineRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]proto.EngineRequest(nil), s.reqs...)
}

func snap(pid int) *contracts.Snapshot {
	return &contracts.Snapshot{PID: pid, At: time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC), Lease: contracts.LeaseStatus{Held: true}}
}

func ctx(t *testing.T) context.Context {
	c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return c
}

func TestStatusStopExtend(t *testing.T) {
	s := newFakeServer(t, func(req proto.EngineRequest, conn net.Conn) {
		resp := proto.EngineResponse{Result: proto.OKResult(), Snapshot: snap(42)}
		if req.Op == proto.OpEngineExtend && req.Minutes == 99 {
			resp = proto.EngineResponse{Result: proto.Fail(proto.CodeNotAbortable, "critical sleep pending")}
		}
		_ = proto.WriteLine(conn, resp)
	})
	c := New(s.path)

	got, err := c.Status(ctx(t))
	if err != nil || got.PID != 42 || !got.Lease.Held {
		t.Fatalf("Status = %+v, %v", got, err)
	}
	yes := true
	if err := c.Stop(ctx(t), &yes); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if _, err := c.Extend(ctx(t), 30, false); err != nil {
		t.Fatalf("Extend: %v", err)
	}
	if _, err := c.Extend(ctx(t), 5, true); err != nil {
		t.Fatalf("Extend forever: %v", err)
	}
	_, err = c.Extend(ctx(t), 99, false)
	if !errors.Is(err, contracts.ErrNotAbortable) {
		t.Fatalf("Extend not abortable: %v", err)
	}

	reqs := s.requests()
	if len(reqs) != 5 {
		t.Fatalf("requests = %+v", reqs)
	}
	if reqs[1].Op != proto.OpEngineStop || reqs[1].Sleep == nil || !*reqs[1].Sleep {
		t.Errorf("stop request = %+v", reqs[1])
	}
	if reqs[2].Minutes != 30 || reqs[2].Forever {
		t.Errorf("extend request = %+v", reqs[2])
	}
	if reqs[3].Minutes != 0 || !reqs[3].Forever {
		t.Errorf("extend forever request = %+v", reqs[3])
	}
}

func TestStopNilSleepOmitsField(t *testing.T) {
	s := newFakeServer(t, func(req proto.EngineRequest, conn net.Conn) {
		_ = proto.WriteLine(conn, proto.EngineResponse{Result: proto.OKResult(), Snapshot: snap(1)})
	})
	if err := New(s.path).Stop(ctx(t), nil); err != nil {
		t.Fatal(err)
	}
	if r := s.requests()[0]; r.Sleep != nil {
		t.Fatalf("sleep = %v, want nil", *r.Sleep)
	}
}

func TestInvalidExtendRejectedLocally(t *testing.T) {
	c := New("/tmp/definitely-not-a-socket-bckp")
	if _, err := c.Extend(ctx(t), 0, false); proto.CodeOf(err) != proto.CodeBadRequest {
		t.Fatalf("err = %v", err)
	}
}

func TestNotRunningMissingSocket(t *testing.T) {
	c := New(filepath.Join(shortDir(t), "engine.sock"))
	if _, err := c.Status(ctx(t)); !errors.Is(err, contracts.ErrEngineNotRunning) {
		t.Fatalf("err = %v", err)
	}
	if _, err := c.Subscribe(ctx(t)); !errors.Is(err, contracts.ErrEngineNotRunning) {
		t.Fatalf("subscribe err = %v", err)
	}
}

func TestNotRunningStaleSocket(t *testing.T) {
	path := filepath.Join(shortDir(t), "engine.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	// Leave the socket file behind, as a crashed engine would.
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	ln.Close()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("socket file should remain: %v", err)
	}
	if _, err := New(path).Status(ctx(t)); !errors.Is(err, contracts.ErrEngineNotRunning) {
		t.Fatalf("err = %v", err)
	}
}

func TestConnectionClosedWithoutResponse(t *testing.T) {
	s := newFakeServer(t, func(proto.EngineRequest, net.Conn) {})
	_, err := New(s.path).Status(ctx(t))
	if err == nil || errors.Is(err, contracts.ErrEngineNotRunning) {
		t.Fatalf("err = %v", err)
	}
}

func TestStatusHonoursContext(t *testing.T) {
	block := make(chan struct{})
	s := newFakeServer(t, func(proto.EngineRequest, net.Conn) { <-block })
	defer close(block)
	c, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := New(s.path).Status(c)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("status did not honour the deadline")
	}
}

func TestSubscribeStreamEndsWithDone(t *testing.T) {
	s := newFakeServer(t, func(req proto.EngineRequest, conn net.Conn) {
		if req.Op != proto.OpEngineSubscribe {
			return
		}
		for i := 1; i <= 3; i++ {
			_ = proto.WriteLine(conn, proto.EngineResponse{Result: proto.OKResult(), Snapshot: snap(i)})
		}
		final := snap(4)
		final.Session.Ended = true
		final.Session.EndReason = contracts.EndStopped
		_ = proto.WriteLine(conn, proto.EngineResponse{Result: proto.OKResult(), Snapshot: final, Done: true})
		// Anything after Done must be ignored.
		_ = proto.WriteLine(conn, proto.EngineResponse{Result: proto.OKResult(), Snapshot: snap(99)})
	})
	ch, err := New(s.path).Subscribe(ctx(t))
	if err != nil {
		t.Fatal(err)
	}
	var pids []int
	var last contracts.Snapshot
	for sn := range ch {
		pids = append(pids, sn.PID)
		last = sn
	}
	if len(pids) != 4 || pids[0] != 1 || pids[3] != 4 {
		t.Fatalf("pids = %v", pids)
	}
	if !last.Session.Ended {
		t.Fatal("last snapshot should be ended")
	}
}

func TestSubscribeCancelClosesChannel(t *testing.T) {
	release := make(chan struct{})
	s := newFakeServer(t, func(req proto.EngineRequest, conn net.Conn) {
		_ = proto.WriteLine(conn, proto.EngineResponse{Result: proto.OKResult(), Snapshot: snap(1)})
		<-release
	})
	defer close(release)
	c, cancel := context.WithCancel(context.Background())
	ch, err := New(s.path).Subscribe(c)
	if err != nil {
		t.Fatal(err)
	}
	if sn := <-ch; sn.PID != 1 {
		t.Fatalf("first = %+v", sn)
	}
	cancel()
	select {
	case _, ok := <-ch:
		if ok {
			// A buffered value is fine; the next receive must be the close.
			if _, ok := <-ch; ok {
				t.Fatal("channel not closed after cancel")
			}
		}
	case <-time.After(2 * time.Second):
		t.Fatal("channel not closed after cancel")
	}
}

func TestSubscribeDropClosesChannel(t *testing.T) {
	s := newFakeServer(t, func(req proto.EngineRequest, conn net.Conn) {
		_ = proto.WriteLine(conn, proto.EngineResponse{Result: proto.OKResult(), Snapshot: snap(1)})
	})
	ch, err := New(s.path).Subscribe(ctx(t))
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for range ch {
		n++
	}
	if n != 1 {
		t.Fatalf("got %d snapshots", n)
	}
}

func TestSubscribeRefused(t *testing.T) {
	s := newFakeServer(t, func(req proto.EngineRequest, conn net.Conn) {
		_ = proto.WriteLine(conn, proto.EngineResponse{Result: proto.Fail(proto.CodeBadRequest, "nope")})
	})
	if _, err := New(s.path).Subscribe(ctx(t)); proto.CodeOf(err) != proto.CodeBadRequest {
		t.Fatalf("err = %v", err)
	}
}

func TestAbortSleep(t *testing.T) {
	calls := 0
	s := newFakeServer(t, func(req proto.EngineRequest, conn net.Conn) {
		calls++
		resp := proto.EngineResponse{Result: proto.OKResult(), Snapshot: snap(7)}
		switch calls {
		case 2:
			resp = proto.EngineResponse{Result: proto.Fail(proto.CodeNoSleepPending, "no sleep pending")}
		case 3:
			resp = proto.EngineResponse{Result: proto.Fail(proto.CodeNotAbortable, "critical sleep pending")}
		}
		_ = proto.WriteLine(conn, resp)
	})
	c := New(s.path)
	if got, err := c.AbortSleep(ctx(t)); err != nil || got.PID != 7 {
		t.Fatalf("AbortSleep = %+v, %v", got, err)
	}
	if _, err := c.AbortSleep(ctx(t)); !errors.Is(err, contracts.ErrNoSleepPending) {
		t.Fatalf("no pending: %v", err)
	}
	if _, err := c.AbortSleep(ctx(t)); !errors.Is(err, contracts.ErrNotAbortable) {
		t.Fatalf("critical: %v", err)
	}
	for _, r := range s.requests() {
		if r.Op != proto.OpEngineAbortSleep || r.Minutes != 0 || r.Forever || r.Sleep != nil {
			t.Fatalf("request = %+v", r)
		}
	}
}
