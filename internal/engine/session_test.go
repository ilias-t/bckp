package engine

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ilias-t/bckp/internal/config"
	"github.com/ilias-t/bckp/internal/contracts"
	"github.com/ilias-t/bckp/internal/fakes"
)

// unplugged is a probe with the phone disconnected: Wi-Fi carries traffic.
func unplugged() contracts.NetSnapshot {
	return contracts.NetSnapshot{DefaultIface: "en0", Iface: "en10", Online: true, WiFiOn: true}
}

// bodies are "title: body" for every notification the engine emitted.
func (h *harness) bodies() []string {
	var out []string
	for _, n := range h.notifs() {
		out = append(out, n.Title+": "+n.Body)
	}
	return out
}

func (h *harness) notifiedBody(substr string) bool {
	return slices.ContainsFunc(h.bodies(), func(s string) bool { return strings.Contains(s, substr) })
}

func autoHarness(t *testing.T, mutate func(*config.Config)) *harness {
	return newHarness(t, func(c *config.Config) {
		c.Network.PreferHotspot = config.HotspotAuto
		if mutate != nil {
			mutate(c)
		}
	})
}

func TestHotspotAutoWaitsForThePhone(t *testing.T) {
	h := autoHarness(t, nil)
	h.net = fakes.NewNetwork(h.clock, testOrder...)
	h.deps.Network = h.net
	h.net.PushSnapshot(unplugged())
	h.start(Options{Choices: []contracts.SessionOption{{Name: contracts.OptionHotspot, Value: "auto", Source: contracts.SourceDefault}}})

	snap := h.e.Snapshot()
	if snap.Network.Mode != contracts.NetworkHotspotAuto || !slices.Equal(h.net.Order(), testOrder) {
		t.Fatalf("unplugged: mode %s, order %v", snap.Network.Mode, h.net.Order())
	}
	if !h.journalExists() {
		t.Fatal("the journal must be written at start, before any change")
	}
	if len(snap.Options) != 1 || snap.Options[0].Value != "auto" {
		t.Fatalf("options = %+v", snap.Options)
	}
	if !h.notifiedBody("Hotspot auto (waiting for the phone), watchdog on.") {
		t.Fatalf("start body: %v", h.bodies())
	}

	// The user reorders services mid-session; then the phone is plugged in.
	userOrder := []string{"Thunderbolt Bridge", "Wi-Fi", "iPhone USB"}
	_ = h.net.SetServiceOrder(context.Background(), userOrder)
	h.net.PushSnapshot(online())
	h.polls(1)
	snap = h.e.Snapshot()
	if snap.Network.Mode != contracts.NetworkPreferHotspot {
		t.Fatalf("plugged in: mode %s", snap.Network.Mode)
	}
	if got := h.net.Order(); slices.Index(got, "iPhone USB") > slices.Index(got, "Wi-Fi") {
		t.Fatalf("plugged in: order %v", got)
	}
	j, err := readJournal(h.paths.RestoreJournal)
	if err != nil || j == nil || !j.Reordered || !slices.Equal(j.ServiceOrder, userOrder) {
		t.Fatalf("journal must record the order being replaced: %+v %v", j, err)
	}

	// Unplugging leaves the order alone and asks for nothing more.
	calls := len(h.net.Calls())
	h.net.PushSnapshot(unplugged())
	h.polls(3)
	for _, c := range h.net.Calls()[calls:] {
		if strings.HasPrefix(c, fakes.MethodSetServiceOrder) {
			t.Fatalf("unplug changed the order: %v", c)
		}
	}
	h.net.PushSnapshot(online())
	h.polls(2)

	h.cancel()
	if err := h.wait(); err != nil {
		t.Fatal(err)
	}
	if got := h.net.Order(); !slices.Equal(got, userOrder) || h.journalExists() {
		t.Fatalf("exit must restore %v, got %v", userOrder, got)
	}
}

func TestHotspotAutoAlreadyAhead(t *testing.T) {
	order := []string{"iPhone USB", "Wi-Fi"}
	h := autoHarness(t, nil)
	h.net = fakes.NewNetwork(h.clock, order...)
	h.deps.Network = h.net
	h.net.PushSnapshot(online())
	h.start(Options{})
	if h.e.Snapshot().Network.Mode != contracts.NetworkPreferHotspot {
		t.Fatal("want prefer_hotspot once connected")
	}
	for _, c := range h.net.Calls() {
		if strings.HasPrefix(c, fakes.MethodSetServiceOrder) {
			t.Fatalf("nothing to change, got %v", c)
		}
	}
	h.cancel()
	h.wait()
}

func TestHotspotAutoReorderFailureDropsPreference(t *testing.T) {
	h := autoHarness(t, func(c *config.Config) { c.Network.Watchdog = false })
	h.net.FailNext(fakes.MethodSetServiceOrder, errors.New("networksetup: denied"))
	h.start(Options{})
	snap := h.e.Snapshot()
	if snap.Network.Mode != contracts.NetworkOff {
		t.Fatalf("with the watchdog off, a failed reorder leaves the network unwatched; mode %s", snap.Network.Mode)
	}
	if !h.notified("Hotspot preference off") || !strings.Contains(snap.Errors["network"], "set service order") {
		t.Fatalf("titles %v, errors %v", h.titles(), snap.Errors)
	}
	h.polls(4)
	n := 0
	for _, c := range h.net.Calls() {
		if strings.HasPrefix(c, fakes.MethodSetServiceOrder) {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("a failed reorder must not be retried every poll: %d attempts", n)
	}
	h.cancel()
	h.wait()
	if !slices.Equal(h.net.Order(), testOrder) || h.journalExists() {
		t.Fatalf("order %v, journal %v", h.net.Order(), h.journalExists())
	}
}

func TestHotspotAutoWithWatchdogOffStillProbes(t *testing.T) {
	h := autoHarness(t, func(c *config.Config) { c.Network.Watchdog = false })
	h.net.PushSnapshot(offline())
	h.start(Options{})
	h.polls(20) // 5 minutes offline: past recovery_after
	for _, c := range h.net.Calls() {
		if strings.HasPrefix(c, fakes.MethodSetServiceEnabled) {
			t.Fatalf("watchdog off must never toggle the service: %v", c)
		}
	}
	if h.net.Snapshots() < 20 {
		t.Fatalf("auto must keep probing: %d probes", h.net.Snapshots())
	}
	h.cancel()
	h.wait()
}

func TestStartNotificationAfterFirstPoll(t *testing.T) {
	h := newHarness(t, nil)
	h.start(Options{Duration: time.Hour})
	var body string
	for _, b := range h.bodies() {
		if strings.HasPrefix(b, "backpack on") {
			body = b
		}
	}
	if body == "" || strings.Contains(body, "?") {
		t.Fatalf("start body must carry a real reading: %q", body)
	}
	if !strings.Contains(body, "thermal nominal") || !strings.Contains(body, "net en10") {
		t.Fatalf("start body = %q", body)
	}
	h.cancel()
	h.wait()
}

func TestAbortSleepKeepsCap(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.Session.MaxDuration = 2 * time.Hour })
	h.start(Options{Duration: 30 * time.Minute})
	before := h.e.Snapshot().Session

	h.do(func() {
		if _, err := h.e.AbortSleep(context.Background()); !errors.Is(err, contracts.ErrNoSleepPending) {
			t.Fatalf("no pending sleep: err = %v", err)
		}
	})
	goSerious(h)
	var snap contracts.Snapshot
	h.do(func() {
		var err error
		snap, err = h.e.AbortSleep(context.Background())
		if err != nil {
			t.Fatal(err)
		}
	})
	if snap.SleepPending != nil || snap.Thermal.FSM != contracts.FSMHot {
		t.Fatalf("abort must cancel the sleep: %+v %s", snap.SleepPending, snap.Thermal.FSM)
	}
	if !snap.Session.EndsAt.Equal(before.EndsAt) || !snap.Session.CapAt.Equal(before.CapAt) || snap.Session.Forever {
		t.Fatalf("abort must not add time or reset the cap: before %+v after %+v", before, snap.Session)
	}
	if !h.notified("Thermal sleep cancelled") || h.notified("backpack extended") {
		t.Fatalf("titles = %v", h.titles())
	}
	h.cancel()
	h.wait()
}

func TestAbortSleepCriticalNotAbortable(t *testing.T) {
	h := newHarness(t, nil)
	h.start(Options{})
	h.pushThermal(contracts.ThermalCritical, 2)
	h.polls(2)
	if p := h.e.Snapshot().SleepPending; p == nil || p.Abortable {
		t.Fatalf("pending = %+v", p)
	}
	h.do(func() {
		if _, err := h.e.AbortSleep(context.Background()); !errors.Is(err, contracts.ErrNotAbortable) {
			t.Fatalf("err = %v", err)
		}
	})
	h.advance(time.Minute)
	h.wait()
	if h.e.Snapshot().Session.EndReason != contracts.EndThermalSleep {
		t.Fatal("the critical sleep must still run")
	}
}

// TestStopAfterEndIgnoresCancelledCtx: with the session over and ctx
// already cancelled, both select cases are ready; the finished stop must
// win every time rather than half the time.
func TestStopAfterEndIgnoresCancelledCtx(t *testing.T) {
	h := newHarness(t, nil)
	h.start(Options{})
	h.do(func() {
		if err := h.e.Stop(context.Background(), nil); err != nil {
			t.Fatal(err)
		}
	})
	h.wait()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for i := range 2000 {
		if err := h.e.Stop(ctx, nil); err != nil {
			t.Fatalf("attempt %d: Stop after the end = %v", i, err)
		}
	}
	if _, err := h.e.Extend(ctx, 5, false); !errors.Is(err, contracts.ErrEngineNotRunning) {
		t.Fatalf("Extend after the end = %v", err)
	}
}
