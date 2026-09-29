package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/ilias-t/bckp/internal/contracts"
	"github.com/ilias-t/bckp/internal/daemon"
	"github.com/ilias-t/bckp/internal/fakes"
	"github.com/ilias-t/bckp/internal/paths"
)

var (
	cmdOff   = paths.BinPmset + " -a disablesleep 0"
	cmdOn    = paths.BinPmset + " -a disablesleep 1"
	cmdSleep = paths.BinPmset + " sleepnow"
	cmdIoreg = paths.BinIoreg + " -r -k AppleClamshellState -d 4"
	budget   = 3 * time.Second
)

// kernel models SleepDisabled for the real daemon under test.
type kernel struct {
	mu     sync.Mutex
	flag   bool
	sleeps int
}

func (k *kernel) Run(ctx context.Context, name string, args ...string) (contracts.ExecResult, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	switch line := contracts.CommandLine(name, args...); line {
	case cmdOn, cmdOff:
		k.flag = line == cmdOn
	case cmdSleep:
		k.sleeps++
	case cmdIoreg:
		v := "No"
		if k.flag {
			v = "Yes"
		}
		return contracts.ExecResult{Stdout: []byte("  |   \"AppleClamshellState\" = Yes\n  |   \"SleepDisabled\" = " + v + "\n")}, nil
	default:
		return contracts.ExecResult{ExitCode: -1}, fmt.Errorf("unexpected %q", line)
	}
	return contracts.ExecResult{}, nil
}

func (k *kernel) state() (flag bool, sleeps int) {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.flag, k.sleeps
}

type env struct {
	t      *testing.T
	sock   string
	k      *kernel
	clock  *fakes.Clock
	cancel context.CancelFunc
	done   chan error
}

func startDaemon(t *testing.T, allowSelf bool) *env {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "bckpc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	uid := os.Getuid()
	if !allowSelf {
		uid++
	}
	allow := filepath.Join(dir, "allowed_uids")
	if err := os.WriteFile(allow, []byte(fmt.Sprintf("%d\n", uid)), 0o644); err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, sock: filepath.Join(dir, "d.sock"), k: &kernel{}, clock: fakes.NewClock(time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC))}
	nominal := fakes.NewThermal(e.clock, contracts.ThermalReading{State: contracts.ThermalNominal})
	power := fakes.NewPower(e.clock, contracts.PowerReading{OnAC: true, Percent: 90})
	s := daemon.New(daemon.Deps{Exec: e.k, Clock: e.clock, Thermal: nominal, Power: power,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))},
		daemon.Options{Socket: e.sock, AllowedUIDs: allow, AllowlistOwner: os.Getuid(), IdleCheck: -1})
	ctx, cancel := context.WithCancel(context.Background())
	e.cancel, e.done = cancel, make(chan error, 1)
	go func() { e.done <- s.Run(ctx) }()
	t.Cleanup(e.stop)
	e.eventually("listening", func() bool {
		c, err := net.Dial("unix", e.sock)
		if err == nil {
			c.Close()
		}
		return err == nil
	})
	return e
}

func (e *env) stop() {
	if e.cancel != nil {
		e.cancel()
		e.cancel = nil
		<-e.done
	}
}

func (e *env) eventually(what string, cond func() bool) {
	e.t.Helper()
	deadline := time.Now().Add(budget)
	for !cond() {
		if time.Now().After(deadline) {
			e.t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func ctx(t *testing.T) context.Context {
	c, cancel := context.WithTimeout(context.Background(), budget)
	t.Cleanup(cancel)
	return c
}

// lostValue waits for Lost to deliver: (err, true) for an error followed
// by close, (nil, true) for a close without a value.
func lostValue(t *testing.T, l contracts.Lease) error {
	t.Helper()
	select {
	case err, ok := <-l.Lost():
		if !ok {
			return nil
		}
		select {
		case _, ok := <-l.Lost():
			if ok {
				t.Fatal("Lost delivered more than one value")
			}
		case <-time.After(budget):
			t.Fatal("Lost not closed after its error")
		}
		return err
	case <-time.After(budget):
		t.Fatal("Lost did not fire")
	}
	return nil
}

func TestAcquireStatusRelease(t *testing.T) {
	e := startDaemon(t, true)
	g := New(e.sock)
	l, err := g.Acquire(ctx(t), 60*time.Second, "backpack 90m")
	if err != nil {
		t.Fatal(err)
	}
	if l.ID() == "" || !l.Expires().Equal(e.clock.Now().Add(60*time.Second)) {
		t.Fatalf("lease %q expires %v", l.ID(), l.Expires())
	}
	st, err := g.Status(ctx(t))
	if err != nil || st.Lease == nil || st.Lease.ID != l.ID() || !st.SleepDisabled || st.Lease.Reason != "backpack 90m" {
		t.Fatalf("status = %+v, %v", st, err)
	}
	if st.Lease.PID != os.Getpid() || st.Lease.UID != os.Getuid() {
		t.Fatalf("peer = uid %d pid %d", st.Lease.UID, st.Lease.PID)
	}
	if err := l.Release(ctx(t)); err != nil {
		t.Fatal(err)
	}
	if err := lostValue(t, l); err != nil {
		t.Fatalf("Lost after Release = %v, want close without value", err)
	}
	if flag, _ := e.k.state(); flag {
		t.Fatal("flag set after release")
	}
	if err := l.Release(ctx(t)); err != nil {
		t.Fatalf("second Release = %v", err)
	}
	if err := l.Renew(ctx(t)); !errors.Is(err, contracts.ErrLeaseLost) {
		t.Fatalf("Renew after Release = %v", err)
	}
	if first, second := l.Lost(), l.Lost(); first != second {
		t.Fatal("Lost returns different channels")
	}
}

func TestBusyAndNotAllowedAndUnavailable(t *testing.T) {
	e := startDaemon(t, true)
	l, err := New(e.sock).Acquire(ctx(t), 0, "a")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Release(ctx(t))
	if _, err := New(e.sock).Acquire(ctx(t), 0, "b"); !errors.Is(err, contracts.ErrLeaseBusy) {
		t.Fatalf("second Acquire = %v", err)
	}
	if err := New(e.sock).SleepNow(ctx(t), "cli"); !errors.Is(err, contracts.ErrLeaseBusy) {
		t.Fatalf("SleepNow by non-holder = %v", err)
	}

	denied := startDaemon(t, false)
	g := New(denied.sock)
	if _, err := g.Acquire(ctx(t), 0, ""); !errors.Is(err, contracts.ErrNotAllowed) {
		t.Fatalf("Acquire denied = %v", err)
	}
	if _, err := g.Status(ctx(t)); !errors.Is(err, contracts.ErrNotAllowed) {
		t.Fatalf("Status denied = %v", err)
	}

	missing := New("/tmp/bckp-no-such.sock")
	if _, err := missing.Status(ctx(t)); !errors.Is(err, contracts.ErrDaemonUnavailable) {
		t.Fatalf("Status unavailable = %v", err)
	}
	if _, err := missing.Acquire(ctx(t), 0, ""); !errors.Is(err, contracts.ErrDaemonUnavailable) {
		t.Fatalf("Acquire unavailable = %v", err)
	}
}

// A lease nobody renews is lost at its TTL: the client never renews on
// its own, so a hung holder cannot keep the Mac awake.
func TestLostOnDaemonExpiry(t *testing.T) {
	e := startDaemon(t, true)
	l, err := New(e.sock).Acquire(ctx(t), 30*time.Second, "")
	if err != nil {
		t.Fatal(err)
	}
	c, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	if err := e.clock.BlockUntilTimers(c, 2); err != nil {
		t.Fatal(err)
	}
	e.clock.Advance(30 * time.Second)
	if err := lostValue(t, l); !errors.Is(err, contracts.ErrLeaseLost) {
		t.Fatalf("Lost = %v", err)
	}
	if err := l.Renew(ctx(t)); !errors.Is(err, contracts.ErrLeaseLost) {
		t.Fatalf("Renew after loss = %v", err)
	}
	if err := l.Release(ctx(t)); err != nil {
		t.Fatalf("Release after loss = %v", err)
	}
}

func TestLostOnDaemonShutdown(t *testing.T) {
	e := startDaemon(t, true)
	l, err := New(e.sock).Acquire(ctx(t), 0, "")
	if err != nil {
		t.Fatal(err)
	}
	e.stop()
	if err := lostValue(t, l); !errors.Is(err, contracts.ErrLeaseLost) {
		t.Fatalf("Lost = %v", err)
	}
}

func TestSleepNowWithLease(t *testing.T) {
	e := startDaemon(t, true)
	g := New(e.sock)
	l, err := g.Acquire(ctx(t), 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := g.SleepNow(ctx(t), "thermal critical"); err != nil {
		t.Fatal(err)
	}
	if err := lostValue(t, l); err != nil {
		t.Fatalf("Lost after SleepNow = %v, want close without value", err)
	}
	e.eventually("sleepnow", func() bool { flag, n := e.k.state(); return !flag && n == 1 })
	// Without a lease, SleepNow uses a fresh connection.
	if err := g.SleepNow(ctx(t), "again"); err != nil {
		t.Fatal(err)
	}
	e.eventually("second sleepnow", func() bool { _, n := e.k.state(); return n == 2 })
}

func TestCancelledRenewDropsLease(t *testing.T) {
	e := startDaemon(t, true)
	l, err := New(e.sock).Acquire(ctx(t), 0, "")
	if err != nil {
		t.Fatal(err)
	}
	c, cancel := context.WithCancel(context.Background())
	cancel()
	if err := l.Renew(c); err == nil {
		t.Fatal("Renew with cancelled ctx succeeded")
	}
	if err := lostValue(t, l); !errors.Is(err, contracts.ErrLeaseLost) {
		t.Fatalf("Lost = %v", err)
	}
	e.eventually("daemon reset", func() bool { flag, _ := e.k.state(); return !flag })
}
