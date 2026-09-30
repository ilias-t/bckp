package engine

import (
	"context"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ilias-t/bckp/internal/config"
	"github.com/ilias-t/bckp/internal/contracts"
	"github.com/ilias-t/bckp/internal/fakes"
	"github.com/ilias-t/bckp/internal/paths"
)

var testOrder = []string{"Wi-Fi", "iPhone USB", "Thunderbolt Bridge"}

// harness runs a real Engine against fakes and drives its fake clock
// through Deps.OnIdle.
type harness struct {
	t        *testing.T
	cfg      config.Config
	clock    *fakes.Clock
	thermal  *fakes.Thermal
	power    *fakes.Power
	net      *fakes.Network
	notifier *fakes.Notifier
	guard    *fakes.SleepGuard
	log      *fakes.Log
	paths    paths.User
	deps     Deps
	e        *Engine

	mu   sync.Mutex
	gen  int
	next time.Time
	sig  chan struct{}

	seen     int
	parked   bool
	parkedAt time.Time

	ctx    context.Context
	cancel context.CancelFunc
	runErr chan error
}

// legacyThermalTiming pins the thermal timings these tests were written
// against (10m hot, 60s grace, 2 calm polls). They test the machinery,
// not the defaults; config_test.go covers the defaults.
func legacyThermalTiming(c *config.Thermal) {
	c.SeriousSleepAfter = 10 * time.Minute
	c.SeriousGrace = 60 * time.Second
	c.CooledPolls = 2
}

func newHarness(t *testing.T, mutate func(*config.Config)) *harness {
	t.Helper()
	cfg := config.Default()
	legacyThermalTiming(&cfg.Thermal)
	// Most tests exercise the watchdog alone; auto has its own tests.
	cfg.Network.PreferHotspot = config.HotspotOff
	// Most tests check what reaches the notifier; the push policy has its
	// own tests (notifyclass_test.go).
	cfg.Notify.Push = config.PushAll
	if mutate != nil {
		mutate(&cfg)
	}
	clock := fakes.NewClock(t0)
	h := &harness{
		t:        t,
		cfg:      cfg,
		clock:    clock,
		thermal:  fakes.NewThermal(clock, calmReading(contracts.ThermalNominal)),
		power:    fakes.NewPower(clock, contracts.PowerReading{OnAC: true, Percent: 100, LidClosed: true, SleepDisabled: true}),
		net:      fakes.NewNetwork(clock, testOrder...),
		notifier: fakes.NewNotifier(),
		guard:    fakes.NewSleepGuard(clock),
		log:      fakes.NewLog(nil),
		paths:    paths.ForHome(t.TempDir()),
		sig:      make(chan struct{}, 1),
		runErr:   make(chan error, 1),
	}
	h.net.PushSnapshot(online())
	h.deps = Deps{
		Clock: clock, Thermal: h.thermal, Power: h.power, Network: h.net, Notifier: h.notifier,
		Guard: h.guard, Config: cfg, Paths: h.paths, Logger: h.log.Logger(),
		LoadAvg: func() (float64, error) { return 1.5, nil },
		OnIdle:  h.onIdle,
	}
	return h
}

func online() contracts.NetSnapshot {
	return contracts.NetSnapshot{DefaultIface: "en10", Iface: "en10", Online: true, IfacePresent: true, GatewayReachable: true, WiFiOn: true}
}

func offline() contracts.NetSnapshot {
	return contracts.NetSnapshot{Iface: "en10", IfacePresent: true, WiFiOn: true, Diag: "route=none"}
}

func (h *harness) onIdle(next time.Time) {
	h.mu.Lock()
	h.gen++
	h.next = next
	h.mu.Unlock()
	select {
	case h.sig <- struct{}{}:
	default:
	}
}

// start runs the engine; it returns once the engine is parked in its loop
// (or Run returned early).
func (h *harness) start(opts Options) {
	h.t.Helper()
	h.e = New(h.deps)
	h.ctx, h.cancel = context.WithCancel(context.Background())
	h.t.Cleanup(h.cancel)
	go func() { h.runErr <- h.e.Run(h.ctx, opts) }()
	h.waitParked()
}

func (h *harness) done() bool {
	select {
	case <-h.e.Done():
		return true
	default:
		return false
	}
}

// waitParked returns the engine's next deadline once it is blocked in its
// loop, or the zero time when Run has returned.
func (h *harness) waitParked() time.Time {
	h.t.Helper()
	if h.parked {
		return h.parkedAt
	}
	timeout := time.After(5 * time.Second)
	for {
		h.mu.Lock()
		if h.gen > h.seen {
			h.seen, h.parked, h.parkedAt = h.gen, true, h.next
			h.mu.Unlock()
			return h.parkedAt
		}
		h.mu.Unlock()
		select {
		case <-h.sig:
		case <-h.e.Done():
			return time.Time{}
		case <-timeout:
			h.t.Fatal("engine did not park within 5s")
		}
	}
}

// advanceTo moves the fake clock to target, letting the engine handle every
// deadline on the way.
func (h *harness) advanceTo(target time.Time) {
	h.t.Helper()
	for {
		next := h.waitParked()
		if next.IsZero() {
			return
		}
		if next.After(target) {
			h.clock.Set(target)
			return
		}
		h.parked = false
		h.clock.Set(next)
	}
}

func (h *harness) advance(d time.Duration) { h.advanceTo(h.clock.Now().Add(d)) }

// polls advances n poll intervals.
func (h *harness) polls(n int) { h.advance(time.Duration(n) * h.cfg.Session.Poll) }

// do runs a command against the parked engine.
func (h *harness) do(fn func()) {
	h.t.Helper()
	h.waitParked()
	h.parked = false
	fn()
}

func (h *harness) wait() error {
	h.t.Helper()
	select {
	case err := <-h.runErr:
		return err
	case <-time.After(5 * time.Second):
		h.t.Fatal("Run did not return within 5s")
		return nil
	}
}

func (h *harness) titles() []string {
	var out []string
	for _, n := range h.notifier.Attempts() {
		out = append(out, n.Title)
	}
	return out
}

func (h *harness) hasTitle(prefix string) bool {
	return slices.ContainsFunc(h.titles(), func(s string) bool { return strings.HasPrefix(s, prefix) })
}

// notifs returns every notification the engine sent or only logged, in
// order, read back from its event log (notify.enqueue and notify.logged
// are written synchronously, unlike the sender goroutine's results).
func (h *harness) notifs() []contracts.Notification {
	var out []contracts.Notification
	for _, r := range h.log.Records() {
		if r.Name == "notify.enqueue" || r.Name == "notify.logged" {
			out = append(out, contracts.Notification{Title: r.Str("title"), Body: r.Str("body"),
				Priority: int(r.Attrs["priority"].Int64()), Class: contracts.NotifyClass(r.Str("class"))})
		}
	}
	return out
}

// notifTitles returns the titles of notifs.
func (h *harness) notifTitles() []string {
	var out []string
	for _, n := range h.notifs() {
		out = append(out, n.Title)
	}
	return out
}

func (h *harness) journalExists() bool {
	_, err := os.Stat(h.paths.RestoreJournal)
	return err == nil
}

func (h *harness) pushThermal(st contracts.ThermalState, n int) {
	for range n {
		h.thermal.Push(calmReading(st))
	}
}

// calmReading is a reading in state st with a calm battery, so the
// battery rules stay quiet unless a test sets BatteryC itself.
func calmReading(st contracts.ThermalState) contracts.ThermalReading {
	b := 30.0
	return contracts.ThermalReading{State: st, BatteryC: &b}
}
