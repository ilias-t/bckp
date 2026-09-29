package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ilias-t/bckp/internal/config"
	"github.com/ilias-t/bckp/internal/contracts"
	"github.com/ilias-t/bckp/internal/fakes"
	"github.com/ilias-t/bckp/internal/notify/outbox"
	"github.com/ilias-t/bckp/internal/proto"
)

func ptr[T any](v T) *T { return &v }

// notified reports whether the engine queued a notification whose title
// starts with prefix (synchronous, unlike the async sender).
func (h *harness) notified(prefix string) bool {
	return slices.ContainsFunc(h.notifTitles(), func(s string) bool { return strings.HasPrefix(s, prefix) })
}

func TestAcquireFailure(t *testing.T) {
	h := newHarness(t, nil)
	h.guard.FailAcquire(contracts.ErrDaemonUnavailable)
	h.start(Options{})
	err := h.wait()
	if !errors.Is(err, contracts.ErrDaemonUnavailable) {
		t.Fatalf("Run err = %v", err)
	}
	snap := h.e.Snapshot()
	if !snap.Session.Ended || snap.Session.EndReason != contracts.EndError || snap.Errors["engine"] == "" {
		t.Fatalf("snapshot = %+v", snap.Session)
	}
	if len(h.notifier.Attempts()) != 0 || h.journalExists() {
		t.Fatal("nothing may happen without a lease")
	}
	if err := h.e.Stop(context.Background(), nil); err != nil {
		t.Fatalf("stop after end = %v", err)
	}
}

func TestRunTwiceAndBeforeRun(t *testing.T) {
	h := newHarness(t, nil)
	e := New(h.deps)
	if err := e.Stop(context.Background(), nil); !errors.Is(err, contracts.ErrEngineNotRunning) {
		t.Fatalf("stop before run = %v", err)
	}
	h.start(Options{})
	if err := h.e.Run(context.Background(), Options{}); err == nil {
		t.Fatal("second Run must fail")
	}
	h.do(func() { _ = h.e.Stop(context.Background(), nil) })
	if err := h.wait(); err != nil {
		t.Fatal(err)
	}
}

func TestStartSnapshotAndStopNil(t *testing.T) {
	h := newHarness(t, nil)
	h.start(Options{Duration: 90 * time.Minute})
	snap := h.e.Snapshot()
	if !snap.Lease.Held || snap.Lease.ID != "lease-1" || snap.PID != os.Getpid() {
		t.Fatalf("lease = %+v", snap.Lease)
	}
	if snap.Session.Forever || !snap.Session.EndsAt.Equal(t0.Add(90*time.Minute)) || !snap.Session.CapAt.Equal(t0.Add(12*time.Hour)) {
		t.Fatalf("session = %+v", snap.Session)
	}
	if snap.Session.RemainingS != 90*60 || snap.Thermal.FSM != contracts.FSMNominal || snap.Network.Mode != contracts.NetworkWatchdog {
		t.Fatalf("snapshot = %+v", snap)
	}
	if acq := h.guard.Acquires(); len(acq) != 1 || acq[0].TTL != proto.DaemonDefaultTTL || !strings.Contains(acq[0].Reason, "until 13:30") {
		t.Fatalf("acquires = %+v", acq)
	}
	if !h.notified("backpack on until 13:30") {
		t.Fatalf("start notification missing: %v", h.notifTitles())
	}

	h.polls(4)
	if got := h.e.Snapshot().Session.RemainingS; got != 89*60 {
		t.Fatalf("remaining after 1m = %d", got)
	}
	if h.guard.Leases()[0].Renews() != 2 {
		t.Fatalf("renews after 60s = %d", h.guard.Leases()[0].Renews())
	}

	sub := h.e.Subscribe(context.Background())
	if first := <-sub; first.Session.Ended {
		t.Fatal("first subscribed snapshot already ended")
	}
	h.do(func() {
		if err := h.e.Stop(context.Background(), nil); err != nil {
			t.Fatal(err)
		}
	})
	snap = h.e.Snapshot()
	if !snap.Session.Ended || snap.Session.EndReason != contracts.EndStopped || snap.Lease.Held {
		t.Fatalf("final = %+v", snap)
	}
	var last contracts.Snapshot
	for s := range sub {
		last = s
	}
	if !last.Session.Ended {
		t.Fatal("subscriber must get the final snapshot before close")
	}
	if err := h.wait(); err != nil {
		t.Fatal(err)
	}
	l := h.guard.Leases()[0]
	if l.Releases() != 1 || len(h.guard.Sleeps()) != 0 {
		t.Fatalf("releases=%d sleeps=%v", l.Releases(), h.guard.Sleeps())
	}
	if titles := h.titles(); titles[0] != "backpack on until 13:30" || titles[len(titles)-1] != "backpack off" {
		t.Fatalf("titles = %q", titles)
	}
	for _, r := range snap.Notifications {
		if r.Status == contracts.NotificationFailed {
			t.Fatalf("record failed: %+v", r)
		}
	}
	// Subscribing after the end yields the final snapshot, then closes.
	after := h.e.Subscribe(context.Background())
	if s, ok := <-after; !ok || !s.Session.Ended {
		t.Fatal("late subscriber must get the final snapshot")
	}
	if _, ok := <-after; ok {
		t.Fatal("late subscription must be closed")
	}
}

func TestStopSleepTrue(t *testing.T) {
	h := newHarness(t, nil)
	h.start(Options{})
	h.polls(2)
	h.do(func() {
		if err := h.e.Stop(context.Background(), ptr(true)); err != nil {
			t.Fatal(err)
		}
	})
	if err := h.wait(); err != nil {
		t.Fatal(err)
	}
	if got := h.guard.Sleeps(); !slices.Equal(got, []string{"backpack stop --sleep"}) {
		t.Fatalf("sleeps = %v", got)
	}
	if h.e.Snapshot().Session.EndReason != contracts.EndUserSleep || h.guard.Leases()[0].Releases() != 0 {
		t.Fatal("stop --sleep must sleep via the daemon, not release")
	}
	if titles := h.titles(); titles[len(titles)-1] != "backpack off - sleeping Mac" {
		t.Fatalf("titles = %q", titles)
	}
}

func TestStopSleepFalseWithoutPending(t *testing.T) {
	h := newHarness(t, nil)
	h.start(Options{})
	h.do(func() {
		if err := h.e.Stop(context.Background(), ptr(false)); err != nil {
			t.Fatal(err)
		}
	})
	h.wait()
	if h.e.Snapshot().Session.EndReason != contracts.EndStopped || len(h.guard.Sleeps()) != 0 {
		t.Fatal("stop --no-sleep must just stop")
	}
}

// goSerious drives serious readings until the serious sleep is pending.
func goSerious(h *harness) {
	h.pushThermal(contracts.ThermalSerious, 1)
	h.polls(1) // HOT
	h.polls(40)
	p := h.e.Snapshot().SleepPending
	if p == nil || p.Trigger != contracts.SleepTriggerSerious || !p.Abortable || p.RemainingS != 60 {
		h.t.Fatalf("pending = %+v", p)
	}
}

func goCritical(h *harness) {
	h.pushThermal(contracts.ThermalCritical, 2)
	h.polls(2)
	p := h.e.Snapshot().SleepPending
	if p == nil || p.Trigger != contracts.SleepTriggerCritical || p.Abortable || p.RemainingS != 15 {
		h.t.Fatalf("pending = %+v", p)
	}
}

func TestSeriousSleepExecutes(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.Network.PreferHotspot = config.HotspotOn })
	h.start(Options{})
	if got := h.net.Order(); got[0] != "iPhone USB" || !h.journalExists() {
		t.Fatalf("hotspot preference not applied: %v", got)
	}
	goSerious(h)
	if h.e.Snapshot().Thermal.FSM != contracts.FSMSleepPending {
		t.Fatal("want SLEEP_PENDING")
	}
	h.advance(60 * time.Second)
	if err := h.wait(); err != nil {
		t.Fatal(err)
	}
	snap := h.e.Snapshot()
	if snap.Session.EndReason != contracts.EndThermalSleep || snap.Thermal.FSM != contracts.FSMSleeping {
		t.Fatalf("final = %+v %s", snap.Session, snap.Thermal.FSM)
	}
	if got := h.guard.Sleeps(); len(got) != 1 || !strings.HasPrefix(got[0], "thermal serious: hot for 10m") {
		t.Fatalf("sleeps = %v", got)
	}
	if !slices.Equal(h.net.Order(), testOrder) || h.journalExists() {
		t.Fatalf("network not restored before sleep: %v", h.net.Order())
	}
	titles := h.titles()
	if !slices.Contains(titles, "Sleeping in 1m: hot for 10m") || titles[len(titles)-1] != "Mac sleeping now (thermal)" {
		t.Fatalf("titles = %q", titles)
	}
	// HOT from the 2nd poll (15s); pending 10m later; asleep 60s after.
	if !t0.Add(15*time.Second + 11*time.Minute).Equal(h.clock.Now()) {
		t.Fatalf("slept at %v", h.clock.Now())
	}
}

func TestExtendAbortsSeriousSleep(t *testing.T) {
	h := newHarness(t, nil)
	h.start(Options{Duration: 30 * time.Minute})
	goSerious(h)
	var snap contracts.Snapshot
	h.do(func() {
		var err error
		snap, err = h.e.Extend(context.Background(), 30, false)
		if err != nil {
			t.Fatal(err)
		}
	})
	now := h.clock.Now()
	if snap.SleepPending != nil || snap.Thermal.FSM != contracts.FSMHot {
		t.Fatalf("extend must cancel the sleep: %+v %s", snap.SleepPending, snap.Thermal.FSM)
	}
	if !snap.Session.EndsAt.Equal(t0.Add(60*time.Minute)) || !snap.Session.CapAt.Equal(now.Add(12*time.Hour)) {
		t.Fatalf("session = %+v", snap.Session)
	}
	if !h.notified("Thermal sleep cancelled") || !h.notified("backpack extended until 13:00") {
		t.Fatalf("notifications = %v", h.notifTitles())
	}
	h.advance(2 * time.Minute)
	if len(h.guard.Sleeps()) != 0 || h.done() {
		t.Fatal("cancelled sleep executed")
	}
	h.do(func() { _ = h.e.Stop(context.Background(), nil) })
	h.wait()
}

func TestStopFalseAbortsSeriousSleep(t *testing.T) {
	h := newHarness(t, nil)
	h.start(Options{})
	goSerious(h)
	h.do(func() {
		if err := h.e.Stop(context.Background(), ptr(false)); err != nil {
			t.Fatal(err)
		}
	})
	h.wait()
	snap := h.e.Snapshot()
	if snap.Session.EndReason != contracts.EndStopped || snap.SleepPending != nil || len(h.guard.Sleeps()) != 0 {
		t.Fatalf("final = %+v sleeps=%v", snap.Session, h.guard.Sleeps())
	}
	if h.guard.Leases()[0].Releases() != 1 {
		t.Fatal("lease must be released")
	}
}

func TestStopNilCancelsAbortableSleep(t *testing.T) {
	h := newHarness(t, nil)
	h.start(Options{})
	goSerious(h)
	h.do(func() { _ = h.e.Stop(context.Background(), nil) })
	h.wait()
	if h.e.Snapshot().Session.EndReason != contracts.EndStopped || len(h.guard.Sleeps()) != 0 {
		t.Fatal("stop must cancel an abortable sleep")
	}
}

func TestCriticalSleepNotAbortable(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.Network.PreferHotspot = config.HotspotOn })
	h.start(Options{})
	goCritical(h)
	h.do(func() {
		if _, err := h.e.Extend(context.Background(), 30, false); !errors.Is(err, contracts.ErrNotAbortable) {
			t.Fatalf("extend = %v", err)
		}
	})
	h.do(func() {
		if err := h.e.Stop(context.Background(), ptr(false)); !errors.Is(err, contracts.ErrNotAbortable) {
			t.Fatalf("stop --no-sleep = %v", err)
		}
	})
	if p := h.e.Snapshot().SleepPending; p == nil || p.Trigger != contracts.SleepTriggerCritical {
		t.Fatalf("refused commands must change nothing: %+v", p)
	}
	h.advance(15 * time.Second)
	if err := h.wait(); err != nil {
		t.Fatal(err)
	}
	if got := h.guard.Sleeps(); len(got) != 1 || !strings.HasPrefix(got[0], "thermal critical") {
		t.Fatalf("sleeps = %v", got)
	}
	if h.e.Snapshot().Session.EndReason != contracts.EndThermalSleep || h.journalExists() || !slices.Equal(h.net.Order(), testOrder) {
		t.Fatal("critical sleep must restore the network and sleep")
	}
}

func TestStopWithCriticalPendingSleepsNow(t *testing.T) {
	for _, sleep := range []*bool{nil, ptr(true)} {
		t.Run(fmt.Sprint(sleep != nil), func(t *testing.T) {
			h := newHarness(t, nil)
			h.start(Options{})
			goCritical(h)
			at := h.clock.Now()
			h.do(func() {
				if err := h.e.Stop(context.Background(), sleep); err != nil {
					t.Fatal(err)
				}
			})
			h.wait()
			if h.e.Snapshot().Session.EndReason != contracts.EndThermalSleep || len(h.guard.Sleeps()) != 1 || !h.clock.Now().Equal(at) {
				t.Fatalf("end=%s sleeps=%v", h.e.Snapshot().Session.EndReason, h.guard.Sleeps())
			}
		})
	}
}

func TestSleepFailureReleasesLease(t *testing.T) {
	h := newHarness(t, nil)
	h.guard.SetSleepErr(errors.New("pmset sleepnow failed"))
	h.start(Options{})
	goCritical(h)
	h.advance(15 * time.Second)
	h.wait()
	if h.guard.Leases()[0].Releases() != 1 || !h.hasTitle("Sleep failed - lease released") {
		t.Fatalf("titles = %v", h.titles())
	}
	if !strings.Contains(h.e.Snapshot().Errors["lease"], "pmset sleepnow failed") {
		t.Fatalf("errors = %v", h.e.Snapshot().Errors)
	}
}

func TestLeaseLost(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.Network.PreferHotspot = config.HotspotOn })
	h.start(Options{})
	h.polls(2)
	h.do(func() { h.guard.Active().Lose(errors.New("connection reset")) })
	err := h.wait()
	if !errors.Is(err, contracts.ErrLeaseLost) {
		t.Fatalf("Run err = %v", err)
	}
	snap := h.e.Snapshot()
	if snap.Session.EndReason != contracts.EndLeaseLost || snap.Lease.Held {
		t.Fatalf("final = %+v", snap.Session)
	}
	if !h.hasTitle("Sleep guard lost") || h.hasTitle("backpack off") {
		t.Fatalf("titles = %v", h.titles())
	}
	if h.journalExists() || !slices.Equal(h.net.Order(), testOrder) {
		t.Fatal("network must be restored after lease loss")
	}
}

func TestRenewRejectedIsLoss(t *testing.T) {
	h := newHarness(t, nil)
	h.start(Options{})
	h.guard.Active().FailRenew(fmt.Errorf("renew: %w", contracts.ErrLeaseLost))
	h.advance(30 * time.Second)
	if err := h.wait(); !errors.Is(err, contracts.ErrLeaseLost) {
		t.Fatalf("Run err = %v", err)
	}
	if h.e.Snapshot().Session.EndReason != contracts.EndLeaseLost {
		t.Fatal("renew rejection must end the session")
	}
}

func TestRenewTransientErrorKeepsRunning(t *testing.T) {
	h := newHarness(t, nil)
	h.start(Options{})
	h.guard.Active().FailRenew(errors.New("timeout"))
	h.advance(30 * time.Second)
	if h.done() || h.e.Snapshot().Errors["lease"] == "" {
		t.Fatalf("transient renew error: done=%v errs=%v", h.done(), h.e.Snapshot().Errors)
	}
	h.advance(30 * time.Second)
	if _, ok := h.e.Snapshot().Errors["lease"]; ok {
		t.Fatal("lease error must clear after a good renew")
	}
	h.cancel()
	h.wait()
}

func TestCtxCancelReleasesAndRestores(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.Network.PreferHotspot = config.HotspotOn })
	h.start(Options{})
	if !h.journalExists() || h.net.Order()[0] != "iPhone USB" {
		t.Fatal("setup")
	}
	j, err := readJournal(h.paths.RestoreJournal)
	if err != nil || !slices.Equal(j.ServiceOrder, testOrder) || !j.Reordered || j.WiFiPower == nil || !*j.WiFiPower {
		t.Fatalf("journal = %+v %v", j, err)
	}
	h.polls(2)
	h.cancel()
	if err := h.wait(); err != nil {
		t.Fatal(err)
	}
	if h.e.Snapshot().Session.EndReason != contracts.EndSignal || h.guard.Leases()[0].Releases() != 1 {
		t.Fatal("ctx cancel must release the lease")
	}
	if h.journalExists() || !slices.Equal(h.net.Order(), testOrder) || !h.net.WiFiOn() {
		t.Fatalf("not restored: order=%v wifi=%v", h.net.Order(), h.net.WiFiOn())
	}
	if !h.hasTitle("backpack off") {
		t.Fatal("stop notification must be flushed despite the cancelled ctx")
	}
}

// panicThermal panics on the nth read.
type panicThermal struct {
	n, reads int
}

func (p *panicThermal) Read(context.Context) (contracts.ThermalReading, error) {
	p.reads++
	if p.reads >= p.n {
		panic("thermal shim exploded")
	}
	return contracts.ThermalReading{State: contracts.ThermalNominal}, nil
}

func TestPanicStillReleasesAndRestores(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.Network.PreferHotspot = config.HotspotOn })
	h.deps.Thermal = &panicThermal{n: 3}
	h.start(Options{})
	h.polls(3)
	err := h.wait()
	if err == nil || !strings.Contains(err.Error(), "panic") {
		t.Fatalf("Run err = %v", err)
	}
	snap := h.e.Snapshot()
	if snap.Session.EndReason != contracts.EndError || !strings.Contains(snap.Errors["engine"], "exploded") {
		t.Fatalf("final = %+v %v", snap.Session, snap.Errors)
	}
	if h.guard.Leases()[0].Releases() != 1 || h.journalExists() || !slices.Equal(h.net.Order(), testOrder) {
		t.Fatal("panic must still release the lease and restore the network")
	}
	if !h.hasTitle("backpack stopped: engine error") {
		t.Fatalf("titles = %v", h.titles())
	}
}

func TestRestoreFailureKeepsJournal(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.Network.PreferHotspot = config.HotspotOn })
	h.start(Options{})
	h.net.FailNext(fakes.MethodSetServiceOrder, errors.New("networksetup: permission denied"))
	h.cancel()
	h.wait()
	if !h.journalExists() || h.net.Order()[0] != "iPhone USB" {
		t.Fatal("journal must be kept when the restore fails")
	}
	if h.guard.Leases()[0].Releases() != 1 {
		t.Fatal("a failed restore must not skip the release")
	}
	if !h.hasTitle("Network restore failed") || !strings.Contains(h.e.Snapshot().Errors["network"], "permission denied") {
		t.Fatalf("titles=%v errs=%v", h.titles(), h.e.Snapshot().Errors)
	}
	restored, err := RestoreStaleJournal(context.Background(), h.net, h.paths)
	if err != nil || !restored || h.journalExists() || !slices.Equal(h.net.Order(), testOrder) {
		t.Fatalf("stale restore = %v %v, order %v", restored, err, h.net.Order())
	}
}

func TestStaleJournalRestoredAtStart(t *testing.T) {
	h := newHarness(t, nil)
	// A previous engine crashed with iPhone USB first and wifi off.
	_ = h.net.SetServiceOrder(context.Background(), []string{"iPhone USB", "Wi-Fi", "Thunderbolt Bridge"})
	_ = h.net.SetWiFiPower(context.Background(), false)
	if err := writeJournal(h.paths.RestoreJournal, Journal{
		Version: JournalVersion, Service: "iPhone USB", ServiceOrder: testOrder, WiFiPower: ptr(true),
		Reordered: true, WiFiChanged: true,
	}); err != nil {
		t.Fatal(err)
	}
	h.start(Options{})
	if !slices.Equal(h.net.Order(), testOrder) || !h.net.WiFiOn() {
		t.Fatalf("stale journal not restored: %v wifi=%v", h.net.Order(), h.net.WiFiOn())
	}
	j, err := readJournal(h.paths.RestoreJournal)
	if err != nil || j == nil || j.Reordered || j.PID != os.Getpid() {
		t.Fatalf("fresh journal = %+v %v", j, err)
	}
	h.cancel()
	h.wait()
	if h.journalExists() {
		t.Fatal("journal must be removed on a clean exit")
	}
}

func TestStaleJournalRestoreFailureDegrades(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.Network.PreferHotspot = config.HotspotOn })
	if err := writeJournal(h.paths.RestoreJournal, Journal{
		Version: JournalVersion, Service: "iPhone USB", ServiceOrder: testOrder, WiFiPower: ptr(true), WiFiChanged: true,
	}); err != nil {
		t.Fatal(err)
	}
	h.net.FailNext(fakes.MethodSetWiFiPower, errors.New("denied"))
	h.start(Options{})
	snap := h.e.Snapshot()
	if snap.Network.Mode != contracts.NetworkWatchdog || !slices.Equal(h.net.Order(), testOrder) {
		t.Fatalf("must degrade to watchdog without reordering: %s %v", snap.Network.Mode, h.net.Order())
	}
	j, _ := readJournal(h.paths.RestoreJournal)
	if j == nil || !j.WiFiChanged {
		t.Fatal("the stale journal must not be overwritten")
	}
	h.cancel()
	h.wait()
	if !h.journalExists() {
		t.Fatal("the stale journal must survive the session")
	}
}

func TestMaxDurationWarningAndEnd(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.Session.MaxDuration = time.Hour })
	h.start(Options{})
	h.advanceTo(t0.Add(50*time.Minute - time.Second))
	if h.notified("backpack ends in") {
		t.Fatal("warned early")
	}
	h.advanceTo(t0.Add(50 * time.Minute))
	if !h.notified("backpack ends in 10m (1h cap)") || !h.e.Snapshot().Session.CapWarned {
		t.Fatalf("notifications = %v", h.notifTitles())
	}
	h.advanceTo(t0.Add(time.Hour))
	if err := h.wait(); err != nil {
		t.Fatal(err)
	}
	if h.e.Snapshot().Session.EndReason != contracts.EndMaxDuration || !h.hasTitle("backpack off: 1h cap reached") {
		t.Fatalf("end = %s titles=%v", h.e.Snapshot().Session.EndReason, h.titles())
	}
}

func TestExtendResetsCap(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.Session.MaxDuration = time.Hour })
	h.start(Options{})
	h.advanceTo(t0.Add(55 * time.Minute))
	var snap contracts.Snapshot
	h.do(func() { snap, _ = h.e.Extend(context.Background(), 0, true) })
	if snap.Session.CapWarned || !snap.Session.CapAt.Equal(t0.Add(115*time.Minute)) || !snap.Session.Forever {
		t.Fatalf("session = %+v", snap.Session)
	}
	h.advanceTo(t0.Add(105 * time.Minute))
	if n := strings.Count(strings.Join(h.notifTitles(), "\n"), "backpack ends in"); n != 2 {
		t.Fatalf("want a second cap warning, got %d", n)
	}
	h.advanceTo(t0.Add(115 * time.Minute))
	h.wait()
	if h.e.Snapshot().Session.EndReason != contracts.EndMaxDuration {
		t.Fatal("want max_duration end")
	}
}

func TestExtendValidation(t *testing.T) {
	h := newHarness(t, nil)
	h.start(Options{Duration: time.Hour})
	for _, m := range []int{0, -1, proto.MaxExtendMinutes + 1} {
		h.do(func() {
			if _, err := h.e.Extend(context.Background(), m, false); err == nil {
				t.Errorf("extend %d must fail", m)
			}
		})
	}
	h.do(func() {
		snap, err := h.e.Extend(context.Background(), 30, false)
		if err != nil || !snap.Session.EndsAt.Equal(t0.Add(90*time.Minute)) {
			t.Errorf("extend = %+v %v", snap.Session, err)
		}
	})
	h.cancel()
	h.wait()
}

func TestForeverStaysForeverOnExtend(t *testing.T) {
	h := newHarness(t, nil)
	h.start(Options{})
	h.do(func() {
		snap, err := h.e.Extend(context.Background(), 30, false)
		if err != nil || !snap.Session.Forever || !snap.Session.EndsAt.IsZero() {
			t.Errorf("extend forever session = %+v %v", snap.Session, err)
		}
	})
	h.cancel()
	h.wait()
}

func TestDurationExpires(t *testing.T) {
	h := newHarness(t, nil)
	h.start(Options{Duration: 30 * time.Minute})
	h.advanceTo(t0.Add(30 * time.Minute))
	h.wait()
	if h.e.Snapshot().Session.EndReason != contracts.EndExpired || h.hasTitle("backpack ends in") {
		t.Fatalf("end = %s titles=%v", h.e.Snapshot().Session.EndReason, h.titles())
	}
	if !h.hasTitle("backpack off: time's up") {
		t.Fatalf("titles = %v", h.titles())
	}
}

func TestBatteryFloor(t *testing.T) {
	h := newHarness(t, nil)
	h.start(Options{})
	h.power.Push(contracts.PowerReading{OnAC: false, Percent: 30, LidClosed: true})
	h.polls(1)
	if !h.notified("On battery (30%)") || h.done() {
		t.Fatal("AC->battery must notify and keep running")
	}
	h.power.Push(contracts.PowerReading{OnAC: false, Percent: 25, LidClosed: true})
	h.polls(1)
	if err := h.wait(); err != nil {
		t.Fatal(err)
	}
	snap := h.e.Snapshot()
	if snap.Session.EndReason != contracts.EndBatteryFloor || len(h.guard.Sleeps()) != 0 || h.guard.Leases()[0].Releases() != 1 {
		t.Fatalf("end=%s sleeps=%v", snap.Session.EndReason, h.guard.Sleeps())
	}
	if !h.hasTitle("Battery 25% - backpack off") || h.hasTitle("backpack off") {
		t.Fatalf("titles = %v", h.titles())
	}
}

func TestNoBatterySkipsRails(t *testing.T) {
	h := newHarness(t, nil)
	h.power = fakes.NewPower(h.clock)
	for range 10 {
		h.power.PushErr(fmt.Errorf("power: %w", contracts.ErrNoBattery))
	}
	h.deps.Power = h.power
	h.start(Options{})
	h.polls(8)
	snap := h.e.Snapshot()
	if _, ok := snap.Errors["power"]; ok || h.notified("Power readings failing") || snap.Power != nil {
		t.Fatalf("no-battery must be quiet: %+v", snap.Errors)
	}
	h.cancel()
	h.wait()
}

func TestRecoveryAndRateLimit(t *testing.T) {
	h := newHarness(t, nil)
	h.start(Options{})
	h.net.PushSnapshot(offline()) // repeats
	h.polls(1)                    // offline at 15s
	if snap := h.e.Snapshot(); snap.Network.Online || !snap.Network.OfflineSince.Equal(t0.Add(15*time.Second)) {
		t.Fatalf("network = %+v", snap.Network)
	}
	h.advanceTo(t0.Add(15*time.Second + 90*time.Second - time.Second))
	if slices.Contains(h.net.Calls(), "SetServiceEnabled iPhone USB false") {
		t.Fatal("recovered before 90s")
	}
	h.advanceTo(t0.Add(105 * time.Second))
	if !slices.Contains(h.net.Calls(), "SetServiceEnabled iPhone USB false") || h.net.Enabled("iPhone USB") {
		t.Fatalf("calls = %v", h.net.Calls())
	}
	j, _ := readJournal(h.paths.RestoreJournal)
	if j == nil || !j.ServiceDisabled {
		t.Fatal("journal must flag the disabled service during the toggle")
	}
	h.advance(RecoveryToggleGap)
	if !h.net.Enabled("iPhone USB") {
		t.Fatal("service must be re-enabled 3s later")
	}
	if j, _ = readJournal(h.paths.RestoreJournal); j.ServiceDisabled {
		t.Fatal("journal flag must clear after re-enable")
	}
	snap := h.e.Snapshot()
	if snap.Network.Recoveries != 1 || !snap.Network.LastRecovery.Equal(t0.Add(105*time.Second)) {
		t.Fatalf("network = %+v", snap.Network)
	}
	count := func() int {
		n := 0
		for _, c := range h.net.Calls() {
			if c == "SetServiceEnabled iPhone USB false" {
				n++
			}
		}
		return n
	}
	h.advanceTo(t0.Add(105*time.Second + 180*time.Second - time.Second))
	if count() != 1 {
		t.Fatalf("rate limit broken: %d toggles", count())
	}
	h.advanceTo(t0.Add(105*time.Second + 180*time.Second))
	if count() != 2 {
		t.Fatalf("second toggle missing: %d", count())
	}
	h.net.PushSnapshot(online())
	h.advance(RecoveryToggleGap)
	h.polls(1)
	if h.notified("Mac back online after 4m45s") {
		t.Fatal("back online must wait one poll")
	}
	h.polls(1)
	if !h.notified("Mac back online after 4m45s") {
		t.Fatalf("notifications = %v", h.notifTitles())
	}
	h.cancel()
	h.wait()
	var body string
	for _, n := range h.notifier.Attempts() {
		if strings.HasPrefix(n.Title, "Mac back online") {
			body = n.Body
		}
	}
	if !strings.Contains(body, "Recovery attempts: 2") || !strings.Contains(body, "first: route=none") {
		t.Fatalf("body = %s", body)
	}
}

func TestStopDuringRecoveryReenablesService(t *testing.T) {
	h := newHarness(t, nil)
	h.start(Options{})
	h.net.PushSnapshot(offline())
	h.advanceTo(t0.Add(105 * time.Second))
	if h.net.Enabled("iPhone USB") {
		t.Fatal("setup: service should be disabled mid-toggle")
	}
	h.do(func() { _ = h.e.Stop(context.Background(), nil) })
	h.wait()
	if !h.net.Enabled("iPhone USB") || h.journalExists() {
		t.Fatal("stop mid-toggle must re-enable the service")
	}
}

// The toggle in flight is undone even when the journal does not record it
// and cannot be written.
func TestStopDuringRecoveryIgnoresJournalFlags(t *testing.T) {
	h := newHarness(t, nil)
	h.start(Options{})
	h.net.PushSnapshot(offline())
	h.advanceTo(t0.Add(105 * time.Second))
	dir := filepath.Dir(h.paths.RestoreJournal)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	h.do(func() {
		h.e.journal.ServiceDisabled = false
		_ = h.e.Stop(context.Background(), nil)
	})
	h.wait()
	if !h.net.Enabled("iPhone USB") {
		t.Fatalf("stop mid-toggle left the service disabled: %v", h.net.Calls())
	}
}

func TestHeartbeat(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.Notify.Heartbeat = 30 * time.Minute })
	h.start(Options{})
	h.advanceTo(t0.Add(30 * time.Minute))
	h.cancel()
	h.wait()
	var hb *contracts.Notification
	for _, n := range h.notifier.Attempts() {
		if n.Title == "Mac: nominal" {
			hb = &n
		}
	}
	if hb == nil || hb.Priority != 2 {
		t.Fatalf("heartbeat = %+v; titles %v", hb, h.titles())
	}
	for _, want := range []string{"thermal nominal", "load 1.50", "AC 100%", "lid closed", "net en10", "forever, cap in 11h30m"} {
		if !strings.Contains(hb.Body, want) {
			t.Errorf("heartbeat body missing %q: %s", want, hb.Body)
		}
	}
}

func TestNotifierErrorsDoNotStopEngine(t *testing.T) {
	h := newHarness(t, nil)
	h.notifier.SetErr(fmt.Errorf("ntfy down: %w", contracts.ErrQueued))
	h.deps.Notifier = pendingOutbox{h.notifier, 3}
	h.start(Options{})
	h.polls(3)
	if h.done() {
		t.Fatal("notifier errors must not stop the engine")
	}
	h.cancel()
	h.wait()
	snap := h.e.Snapshot()
	if len(snap.Notifications) == 0 || snap.Notifications[0].Status != contracts.NotificationQueued || snap.OutboxPending != 3 {
		t.Fatalf("records = %+v pending=%d", snap.Notifications, snap.OutboxPending)
	}
	if !strings.Contains(snap.Errors["notify"], "ntfy down") {
		t.Fatalf("errors = %v", snap.Errors)
	}
}

func TestNotifierFailureRecorded(t *testing.T) {
	h := newHarness(t, nil)
	h.notifier.SetErr(errors.New("ntfy 500"))
	h.start(Options{})
	h.cancel()
	h.wait()
	for _, r := range h.e.Snapshot().Notifications {
		if r.Status != contracts.NotificationFailed || r.Err != "ntfy 500" {
			t.Fatalf("record = %+v", r)
		}
	}
}

func TestSlowSubscriberCoalesced(t *testing.T) {
	h := newHarness(t, nil)
	h.start(Options{})
	sub := h.e.Subscribe(context.Background())
	h.polls(20) // never read meanwhile: must not block the engine
	s := <-sub
	if !s.At.Equal(h.clock.Now()) {
		t.Fatalf("coalesced snapshot is stale: %v vs %v", s.At, h.clock.Now())
	}
	ctx, cancel := context.WithCancel(context.Background())
	sub2 := h.e.Subscribe(ctx)
	<-sub2
	cancel()
	for range sub2 {
	}
	h.cancel()
	h.wait()
	var last contracts.Snapshot
	for s := range sub {
		last = s
	}
	if !last.Session.Ended {
		t.Fatal("final snapshot must win the coalescing")
	}
}

func TestThermalReadErrorsReported(t *testing.T) {
	h := newHarness(t, nil)
	h.start(Options{})
	for range 4 {
		h.thermal.PushErr(errors.New("shim failed"))
	}
	h.thermal.Push(contracts.ThermalReading{State: contracts.ThermalNominal})
	h.polls(4)
	if !h.notified("Thermal readings failing") || h.e.Snapshot().Errors["thermal"] != "shim failed" {
		t.Fatalf("errors = %v", h.e.Snapshot().Errors)
	}
	h.polls(1)
	if _, ok := h.e.Snapshot().Errors["thermal"]; ok || !h.notified("Thermal readings back") {
		t.Fatal("thermal error must clear")
	}
	h.cancel()
	h.wait()
}

func TestNetworkOffMode(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.Network.Watchdog = false })
	h.deps.Network = nil
	h.start(Options{})
	h.polls(3)
	h.cancel()
	h.wait()
	snap := h.e.Snapshot()
	if snap.Network.Mode != contracts.NetworkOff || snap.Network.Latest != nil || h.journalExists() {
		t.Fatalf("network = %+v", snap.Network)
	}
}

// pendingOutbox is a notifier with an outbox holding pending messages that
// never flush.
type pendingOutbox struct {
	*fakes.Notifier
	pending int
}

func (o pendingOutbox) Pending() int                    { return o.pending }
func (o pendingOutbox) Flush(ctx context.Context) error { return errors.New("offline") }

// TestRecoveryRefusedWithoutJournal: a recovery that cannot be journaled
// never disables the service, so stopping in what would be the middle of
// the toggle leaves it enabled.
func TestRecoveryRefusedWithoutJournal(t *testing.T) {
	h, sink := newLoggedHarness(t, nil)
	h.start(Options{})
	dir := filepath.Dir(h.paths.RestoreJournal)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	h.net.PushSnapshot(offline())
	h.advanceTo(t0.Add(105 * time.Second)) // the recovery is due
	h.do(func() { _ = h.e.Stop(context.Background(), nil) })
	h.wait()
	if !h.net.Enabled("iPhone USB") || slices.Contains(h.net.Calls(), "SetServiceEnabled iPhone USB false") {
		t.Fatalf("service touched without a journal: %v", h.net.Calls())
	}
	if ev := sink.one(t, "net.recovery_attempt"); attr(ev, "result") != "refused" {
		t.Fatalf("net.recovery_attempt = %s", ev.Raw)
	}
	if n := h.e.Snapshot().Network.Recoveries; n != 0 {
		t.Fatalf("a refused recovery counted: %d", n)
	}
}

// TestPollReadsProvidersConcurrently: a poll waits for the slowest
// provider, not for their sum.
func TestPollReadsProvidersConcurrently(t *testing.T) {
	h := newHarness(t, nil)
	probed := make(chan struct{}, 1)
	h.deps.Network = probeSignal{Network: h.net, probed: probed}
	h.deps.Thermal = waitThermal{Thermal: h.thermal, wait: probed}
	h.start(Options{})
	h.cancel()
	h.wait()
	if _, ok := h.e.Snapshot().Errors["thermal"]; ok {
		t.Fatalf("thermal read did not overlap the network probe: %v", h.e.Snapshot().Errors)
	}
}

type probeSignal struct {
	*fakes.Network
	probed chan<- struct{}
}

func (p probeSignal) Snapshot(ctx context.Context) (contracts.NetSnapshot, error) {
	select {
	case p.probed <- struct{}{}:
	default:
	}
	return p.Network.Snapshot(ctx)
}

// waitThermal reads only once the network probe started.
type waitThermal struct {
	*fakes.Thermal
	wait <-chan struct{}
}

func (w waitThermal) Read(ctx context.Context) (contracts.ThermalReading, error) {
	select {
	case <-w.wait:
	case <-time.After(2 * time.Second):
		return contracts.ThermalReading{}, errors.New("read alone")
	}
	return w.Thermal.Read(ctx)
}

// TestCutOffNotificationsSpillToOutbox: notifications still unsent at the
// flush deadline, in flight or never attempted, are kept by the outbox
// instead of being lost.
func TestCutOffNotificationsSpillToOutbox(t *testing.T) {
	h := newHarness(t, nil)
	stuck := &stuckNotifier{}
	ob := outbox.New(t.TempDir(), stuck, h.clock)
	h.deps.Notifier = ob
	h.start(Options{})
	h.do(func() { _ = h.e.Stop(context.Background(), nil) })
	h.wait()
	// "backpack on" was in flight at the deadline; "backpack off" was
	// never attempted.
	if n := ob.Pending(); n != 2 {
		t.Fatalf("outbox holds %d, want 2", n)
	}
	for _, r := range h.e.Snapshot().Notifications {
		if r.Status != contracts.NotificationQueued {
			t.Fatalf("record %q: %s", r.Notification.Title, r.Status)
		}
	}
}

// stuckNotifier never delivers: each Send waits for its ctx.
type stuckNotifier struct{}

func (*stuckNotifier) Send(ctx context.Context, _ contracts.Notification) error {
	<-ctx.Done()
	return ctx.Err()
}
