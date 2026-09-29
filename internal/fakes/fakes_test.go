package fakes

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/ilias-t/bckp/internal/contracts"
)

var t0 = time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)

func ctxT(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestClockAfterAndAdvance(t *testing.T) {
	c := NewClock(t0)
	a := c.After(15 * time.Second)
	b := c.After(30 * time.Second)
	if c.PendingTimers() != 2 {
		t.Fatalf("pending = %d", c.PendingTimers())
	}
	c.Advance(10 * time.Second)
	select {
	case <-a:
		t.Fatal("fired early")
	default:
	}
	c.Advance(5 * time.Second)
	if got := <-a; !got.Equal(t0.Add(15 * time.Second)) {
		t.Fatalf("a fired with %v", got)
	}
	c.Advance(time.Minute)
	if got := <-b; !got.Equal(t0.Add(30 * time.Second)) {
		t.Fatalf("b fired with %v", got)
	}
	if c.PendingTimers() != 0 {
		t.Fatal("no timers should remain")
	}
	select {
	case <-c.After(0):
	default:
		t.Fatal("After(0) must fire immediately")
	}
	c.Set(t0) // backwards: ignored
	if !c.Now().Equal(t0.Add(75 * time.Second)) {
		t.Fatal("clock went backwards")
	}
}

func TestClockBlockUntilTimers(t *testing.T) {
	c := NewClock(t0)
	ctx := ctxT(t)
	ticks := make(chan time.Time)
	go func() { // a poll loop parked on the clock
		for i := 0; i < 3; i++ {
			ticks <- <-c.After(15 * time.Second)
		}
	}()
	for i := 1; i <= 3; i++ {
		if err := c.BlockUntilTimers(ctx, 1); err != nil {
			t.Fatal(err)
		}
		c.Advance(15 * time.Second)
		if got := <-ticks; !got.Equal(t0.Add(time.Duration(i) * 15 * time.Second)) {
			t.Fatalf("tick %d = %v", i, got)
		}
	}
}

func TestExec(t *testing.T) {
	e := NewExec().
		On("/usr/bin/pmset -g batt", "first").
		On("/usr/bin/pmset -g batt", "second").
		OnFail("/usr/sbin/networksetup -setairportpower en0 on", 1, "boom")
	ctx := context.Background()
	for _, want := range []string{"first", "second", "second"} {
		res, err := e.Run(ctx, "/usr/bin/pmset", "-g", "batt")
		if err != nil || string(res.Stdout) != want {
			t.Fatalf("got %q, %v; want %q", res.Stdout, err, want)
		}
	}
	_, err := e.Run(ctx, "/usr/sbin/networksetup", "-setairportpower", "en0", "on")
	var ee *contracts.ExitError
	if !errors.As(err, &ee) || ee.ExitCode != 1 {
		t.Fatalf("err = %v", err)
	}
	if _, err := e.Run(ctx, "/usr/bin/pmset", "-a", "disablesleep", "1"); !errors.Is(err, ErrUnexpectedCommand) {
		t.Fatalf("unmatched err = %v", err)
	}
	if e.CallCount("/usr/bin/pmset -g batt") != 3 || len(e.Calls()) != 5 {
		t.Fatalf("calls = %v", e.Calls())
	}
}

func TestThermalScript(t *testing.T) {
	c := NewClock(t0)
	src := NewThermal(c, contracts.ThermalReading{State: contracts.ThermalNominal}, contracts.ThermalReading{State: contracts.ThermalSerious})
	src.PushErr(errors.New("shim failed"))
	src.Push(contracts.ThermalReading{State: contracts.ThermalCritical})
	ctx := context.Background()

	r, err := src.Read(ctx)
	if err != nil || r.State != contracts.ThermalNominal || !r.At.Equal(t0) {
		t.Fatalf("read1 = %+v, %v", r, err)
	}
	c.Advance(15 * time.Second)
	if r, _ = src.Read(ctx); r.State != contracts.ThermalSerious || !r.At.Equal(t0.Add(15*time.Second)) {
		t.Fatalf("read2 = %+v", r)
	}
	if _, err = src.Read(ctx); err == nil {
		t.Fatal("expected scripted error")
	}
	c.Advance(15 * time.Second)
	for i := 0; i < 2; i++ { // repeat-last with fresh timestamps
		c.Advance(time.Second)
		r, err = src.Read(ctx)
		if err != nil || r.State != contracts.ThermalCritical || !r.At.Equal(c.Now()) {
			t.Fatalf("repeat %d = %+v, %v", i, r, err)
		}
	}
	if src.Reads() != 5 {
		t.Fatalf("reads = %d", src.Reads())
	}
	if _, err := NewThermal(nil).Read(ctx); !errors.Is(err, errNothingScripted) {
		t.Fatalf("empty source err = %v", err)
	}
}

func TestPowerScript(t *testing.T) {
	src := NewPower(nil,
		contracts.PowerReading{OnAC: true, Percent: 90},
		contracts.PowerReading{OnAC: false, Percent: 24},
	)
	ctx := context.Background()
	if r, _ := src.Read(ctx); !r.OnAC {
		t.Fatal("want AC")
	}
	if r, _ := src.Read(ctx); r.OnAC || r.Percent != 24 {
		t.Fatalf("got %+v", r)
	}
}

func TestNetwork(t *testing.T) {
	n := NewNetwork(nil, "Wi-Fi", "iPhone USB", "Thunderbolt Bridge")
	ctx := context.Background()
	n.PushSnapshot(contracts.NetSnapshot{Online: false, IfacePresent: true})
	if s, err := n.Snapshot(ctx); err != nil || s.Online || !s.IfacePresent {
		t.Fatalf("snapshot = %+v, %v", s, err)
	}
	if err := n.SetServiceOrder(ctx, []string{"iPhone USB", "Wi-Fi", "Thunderbolt Bridge"}); err != nil {
		t.Fatal(err)
	}
	if got := n.Order(); got[0] != "iPhone USB" {
		t.Fatalf("order = %v", got)
	}
	if err := n.SetServiceOrder(ctx, []string{"iPhone USB"}); err == nil {
		t.Fatal("partial order must fail")
	}
	if err := n.SetServiceEnabled(ctx, "iPhone USB", false); err != nil || n.Enabled("iPhone USB") {
		t.Fatalf("disable: %v", err)
	}
	_ = n.SetServiceEnabled(ctx, "iPhone USB", true)
	if err := n.SetServiceEnabled(ctx, "Nope", true); err == nil {
		t.Fatal("unknown service must fail")
	}
	n.FailNext(MethodSetWiFiPower, errors.New("no permission"))
	if err := n.SetWiFiPower(ctx, false); err == nil || !n.WiFiOn() {
		t.Fatal("injected failure must not change state")
	}
	if err := n.SetWiFiPower(ctx, false); err != nil || n.WiFiOn() {
		t.Fatal("wifi should be off")
	}
	wantPrefix := []string{
		"Snapshot",
		"SetServiceOrder [iPhone USB Wi-Fi Thunderbolt Bridge]",
		"SetServiceOrder [iPhone USB]",
		"SetServiceEnabled iPhone USB false",
		"SetServiceEnabled iPhone USB true",
	}
	if got := n.Calls(); !slices.Equal(got[:len(wantPrefix)], wantPrefix) {
		t.Fatalf("calls = %q", got)
	}
}

func TestNotifier(t *testing.T) {
	n := NewNotifier()
	ctx := ctxT(t)
	n.FailNext(errors.New("ntfy down"))
	if err := n.Send(ctx, contracts.Notification{Priority: 4, Title: "Hot"}); err == nil {
		t.Fatal("expected failure")
	}
	if err := n.Send(ctx, contracts.Notification{Priority: 3, Title: "Fair", Tags: []string{"warning"}}); err != nil {
		t.Fatal(err)
	}
	if got := n.Sent(); len(got) != 1 || got[0].Title != "Fair" {
		t.Fatalf("sent = %+v", got)
	}
	if len(n.Attempts()) != 2 {
		t.Fatalf("attempts = %d", len(n.Attempts()))
	}
	n.SetErr(errors.New("offline"))
	if err := n.Send(ctx, contracts.Notification{Title: "x"}); err == nil {
		t.Fatal("SetErr should fail sends")
	}
}

func TestSleepGuardLeaseLifecycle(t *testing.T) {
	c := NewClock(t0)
	g := NewSleepGuard(c)
	ctx := context.Background()

	l, err := g.Acquire(ctx, 90*time.Second, "backpack 90m")
	if err != nil {
		t.Fatal(err)
	}
	if !l.Expires().Equal(t0.Add(90 * time.Second)) {
		t.Fatalf("expires = %v", l.Expires())
	}
	if _, err := g.Acquire(ctx, 90*time.Second, "second"); !errors.Is(err, contracts.ErrLeaseBusy) {
		t.Fatalf("second acquire err = %v", err)
	}
	st, _ := g.Status(ctx)
	if !st.SleepDisabled || st.Lease == nil || st.Lease.ID != l.ID() || st.Backstop != contracts.BackstopArmed {
		t.Fatalf("status = %+v", st)
	}
	c.Advance(30 * time.Second)
	if err := l.Renew(ctx); err != nil || !l.Expires().Equal(t0.Add(120*time.Second)) {
		t.Fatalf("renew: %v, expires %v", err, l.Expires())
	}
	if err := l.Release(ctx); err != nil {
		t.Fatal(err)
	}
	if err, ok := <-l.Lost(); ok || err != nil {
		t.Fatalf("release must close Lost without a value, got %v %v", err, ok)
	}
	if err := l.Renew(ctx); !errors.Is(err, contracts.ErrLeaseLost) {
		t.Fatalf("renew after release = %v", err)
	}
	st, _ = g.Status(ctx)
	if st.SleepDisabled || st.Lease != nil || st.Backstop != contracts.BackstopIdle {
		t.Fatalf("status after release = %+v", st)
	}
}

func TestSleepGuardLoseAndSleep(t *testing.T) {
	g := NewSleepGuard(nil)
	ctx := context.Background()

	l, _ := g.Acquire(ctx, 90*time.Second, "a")
	fl := g.Active()
	fl.Lose(errors.New("connection reset"))
	err, ok := <-l.Lost()
	if !ok || !errors.Is(err, contracts.ErrLeaseLost) {
		t.Fatalf("lost = %v %v", err, ok)
	}
	if _, ok := <-l.Lost(); ok {
		t.Fatal("Lost must close after the error")
	}

	l2, err := g.Acquire(ctx, 90*time.Second, "b")
	if err != nil {
		t.Fatalf("re-acquire after loss: %v", err)
	}
	if err := g.SleepNow(ctx, "thermal critical"); err != nil {
		t.Fatal(err)
	}
	if err, ok := <-l2.Lost(); ok || err != nil {
		t.Fatalf("SleepNow must end the lease cleanly, got %v %v", err, ok)
	}
	if !g.Slept() || !slices.Equal(g.Sleeps(), []string{"thermal critical"}) {
		t.Fatalf("sleeps = %v", g.Sleeps())
	}
	if len(g.Acquires()) != 2 || g.Acquires()[1].Reason != "b" {
		t.Fatalf("acquires = %+v", g.Acquires())
	}
	g.FailAcquire(contracts.ErrDaemonUnavailable)
	if _, err := g.Acquire(ctx, time.Minute, "c"); !errors.Is(err, contracts.ErrDaemonUnavailable) {
		t.Fatalf("injected acquire err = %v", err)
	}
}

func TestEngineClient(t *testing.T) {
	ctx := ctxT(t)
	e := NewEngineClient(contracts.Snapshot{PID: 1, Thermal: contracts.ThermalStatus{FSM: contracts.FSMNominal}})
	ch, err := e.Subscribe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if s := <-ch; s.Thermal.FSM != contracts.FSMNominal {
		t.Fatalf("first = %+v", s)
	}
	e.Push(contracts.Snapshot{PID: 1, Thermal: contracts.ThermalStatus{FSM: contracts.FSMHot}})
	if s := <-ch; s.Thermal.FSM != contracts.FSMHot {
		t.Fatalf("pushed = %+v", s)
	}
	yes := true
	_ = e.Stop(ctx, &yes)
	_ = e.Stop(ctx, nil)
	if _, err := e.Extend(ctx, 30, false); err != nil {
		t.Fatal(err)
	}
	if st := e.Stops(); len(st) != 2 || !*st[0] || st[1] != nil {
		t.Fatalf("stops = %v", st)
	}
	if ex := e.Extends(); len(ex) != 1 || ex[0].Minutes != 30 {
		t.Fatalf("extends = %v", ex)
	}

	e.End(contracts.EndStopped)
	final, ok := <-ch
	if !ok || !final.Session.Ended || final.Session.EndReason != contracts.EndStopped {
		t.Fatalf("final = %+v %v", final, ok)
	}
	if _, ok := <-ch; ok {
		t.Fatal("channel must close after End")
	}
	if _, err := e.Status(ctx); !errors.Is(err, contracts.ErrEngineNotRunning) {
		t.Fatalf("status after end = %v", err)
	}
}

func TestEngineClientSubscribeCancel(t *testing.T) {
	e := NewEngineClient(contracts.Snapshot{})
	ctx, cancel := context.WithCancel(context.Background())
	ch, _ := e.Subscribe(ctx)
	<-ch
	cancel()
	if _, ok := <-ch; ok {
		t.Fatal("channel must close on cancel")
	}
	if e.Subscribers() != 0 {
		t.Fatalf("subscribers = %d", e.Subscribers())
	}
}
