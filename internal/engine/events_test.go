package engine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ilias-t/bckp/internal/config"
	"github.com/ilias-t/bckp/internal/contracts"
	"github.com/ilias-t/bckp/internal/eventlog"
	"github.com/ilias-t/bckp/internal/version"
)

// eventSink captures the engine's structured log through the real
// eventlog JSON handler and parses it back.
type eventSink struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *eventSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *eventSink) raw() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

func (s *eventSink) events(t *testing.T) []eventlog.Event {
	t.Helper()
	var out []eventlog.Event
	for _, l := range strings.Split(strings.TrimSpace(s.raw()), "\n") {
		if l == "" {
			continue
		}
		ev, err := eventlog.ParseLine([]byte(l))
		if err != nil {
			t.Fatalf("unparseable log line %q: %v", l, err)
		}
		out = append(out, ev)
	}
	return out
}

func (s *eventSink) named(t *testing.T, name string) []eventlog.Event {
	t.Helper()
	var out []eventlog.Event
	for _, ev := range s.events(t) {
		if ev.Name == name {
			out = append(out, ev)
		}
	}
	return out
}

func (s *eventSink) one(t *testing.T, name string) eventlog.Event {
	t.Helper()
	evs := s.named(t, name)
	if len(evs) == 0 {
		t.Fatalf("no %s event; got %v", name, s.names(t))
	}
	return evs[0]
}

func (s *eventSink) names(t *testing.T) []string {
	var out []string
	for _, ev := range s.events(t) {
		out = append(out, ev.Name)
	}
	return out
}

func attr(ev eventlog.Event, key string) string {
	v, ok := ev.Get(key)
	if !ok {
		return "<missing>"
	}
	return fmt.Sprint(v)
}

const secretTopic = "s3cret-topic-9x"

func newLoggedHarness(t *testing.T, mutate func(*config.Config)) (*harness, *eventSink) {
	h := newHarness(t, func(c *config.Config) {
		c.Notify.NtfyTopic = secretTopic
		if mutate != nil {
			mutate(c)
		}
	})
	sink := &eventSink{}
	// The handler, as wired by runEngine, is the only redactor.
	json := eventlog.NewHandler(sink, eventlog.Options{Redact: []string{secretTopic}})
	h.deps.Logger = slog.New(slog.NewMultiHandler(h.log, json)).With("src", eventlog.SrcEngine)
	return h, sink
}

func TestEventsSessionLifecycle(t *testing.T) {
	h, sink := newLoggedHarness(t, nil)
	h.start(Options{Duration: 2 * time.Hour})
	h.pushThermal(contracts.ThermalSerious, 1)
	h.polls(2)
	h.do(func() {
		if _, err := h.e.Extend(context.Background(), 30, false); err != nil {
			t.Error(err)
		}
	})
	h.do(func() {
		if err := h.e.Stop(context.Background(), nil); err != nil {
			t.Error(err)
		}
	})
	h.wait()

	start := sink.one(t, "session.start")
	if attr(start, "version") != version.String() || start.Src != "engine" {
		t.Fatalf("session.start = %s", start.Raw)
	}
	if cfg := attr(start, "config"); !strings.Contains(cfg, "ntfy:true") {
		t.Fatalf("config summary = %s", cfg)
	}
	if strings.Contains(sink.raw(), secretTopic) {
		t.Fatal("the ntfy topic leaked into the event log")
	}
	if got := attr(sink.one(t, "lease.acquire"), "ttl"); got != "1m30s" {
		t.Fatalf("lease.acquire ttl = %s", got)
	}
	pw := sink.one(t, "power.transition")
	if attr(pw, "from") != "unknown" || attr(pw, "to") != "AC" || attr(pw, "lid") != "closed" {
		t.Fatalf("power.transition = %s", pw.Raw)
	}
	th := sink.one(t, "thermal.transition")
	if attr(th, "from") != "NOMINAL" || attr(th, "to") != "HOT" || attr(th, "thermal") != "serious" {
		t.Fatalf("thermal.transition = %s", th.Raw)
	}
	s := sink.one(t, "sample")
	for _, k := range []string{"thermal", "load", "battery", "ac", "lid", "sleep_disabled", "net", "online", "left"} {
		if attr(s, k) == "<missing>" {
			t.Errorf("sample lacks %s: %s", k, s.Raw)
		}
	}
	if attr(sink.one(t, "session.extend"), "minutes") != "30" {
		t.Fatal("session.extend minutes")
	}
	if attr(sink.one(t, "session.stop"), "sleep") != "default" {
		t.Fatal("session.stop sleep")
	}
	end := sink.one(t, "session.end")
	if attr(end, "reason") != string(contracts.EndStopped) || attr(end, "ran") == "<missing>" {
		t.Fatalf("session.end = %s", end.Raw)
	}
	if attr(sink.one(t, "lease.release"), "result") != "ok" {
		t.Fatal("lease.release result")
	}
	if len(sink.named(t, "notify.sent")) == 0 {
		t.Fatal("no notify.sent")
	}
	// session.start precedes the lease; session.end is the last record.
	names := sink.names(t)
	if names[0] != "session.start" || names[len(names)-1] != "session.end" {
		t.Fatalf("order: %v", names)
	}
}

func TestEventsSampleOncePerMinute(t *testing.T) {
	h, sink := newLoggedHarness(t, nil)
	h.start(Options{})
	h.advance(4*time.Minute + time.Second)
	h.cancel()
	h.wait()
	// Polls at 0s..4m (every 15s): samples at 0, 1m, 2m, 3m, 4m.
	if n := len(sink.named(t, "sample")); n != 5 {
		t.Fatalf("%d samples in 4m, want 5", n)
	}
}

func TestEventsSuspectedSleep(t *testing.T) {
	h, sink := newLoggedHarness(t, nil)
	h.start(Options{})
	h.polls(1)
	h.waitParked()
	h.parked = false
	h.clock.Set(h.clock.Now().Add(10 * time.Minute)) // the Mac was asleep
	h.waitParked()
	h.cancel()
	h.wait()
	ev := sink.one(t, "suspected_sleep")
	if ev.Level != slog.LevelWarn || !strings.HasPrefix(attr(ev, "gap"), "10m") || attr(ev, "expected") != "15s" {
		t.Fatalf("suspected_sleep = %s", ev.Raw)
	}
}

func TestEventsProviderErrorsRateLimited(t *testing.T) {
	h, sink := newLoggedHarness(t, nil)
	h.start(Options{}) // first poll ok
	for range 6 {
		h.thermal.PushErr(errors.New("shim: no reading"))
	}
	h.thermal.Push(contracts.ThermalReading{State: contracts.ThermalNominal})
	h.polls(7) // 6 failures at 15s..90s, then a good reading
	h.cancel()
	h.wait()
	errs := sink.named(t, "provider.error")
	if len(errs) != 2 {
		t.Fatalf("%d provider.error events for 6 failures over 75s, want 2 (one per minute)", len(errs))
	}
	if attr(errs[0], "kind") != "thermal" || attr(errs[1], "suppressed") != "3" || attr(errs[1], "failures") != "5" {
		t.Fatalf("provider.error = %s / %s", errs[0].Raw, errs[1].Raw)
	}
	rec := sink.one(t, "provider.recovered")
	if attr(rec, "failures") != "6" {
		t.Fatalf("provider.recovered = %s", rec.Raw)
	}
}

func TestEventsNotifyFailureRedactsTopic(t *testing.T) {
	h, sink := newLoggedHarness(t, nil)
	h.notifier.SetErr(fmt.Errorf(`Post "https://ntfy.sh/%s": dial tcp: no route to host`, secretTopic))
	h.start(Options{})
	h.cancel()
	h.wait()
	f := sink.one(t, "notify.failed")
	if f.Level != slog.LevelError || attr(f, "title") == "<missing>" || attr(f, "priority") == "<missing>" {
		t.Fatalf("notify.failed = %s", f.Raw)
	}
	if !strings.Contains(attr(f, "err"), "<redacted>") || strings.Contains(sink.raw(), secretTopic) {
		t.Fatalf("topic not redacted: %s", sink.raw())
	}
	if h.e.Snapshot().Errors["notify"] == "" {
		t.Fatal("snapshot error must still be recorded")
	}
}

func TestEventsNetworkOutageAndRecovery(t *testing.T) {
	h, sink := newLoggedHarness(t, nil)
	h.start(Options{})
	h.net.PushSnapshot(offline())
	h.advanceTo(t0.Add(105*time.Second + RecoveryToggleGap))
	h.net.PushSnapshot(online())
	h.polls(1)
	h.cancel()
	h.wait()

	tr := sink.named(t, "net.transition")
	if len(tr) != 2 || attr(tr[0], "to") != "offline" || attr(tr[1], "to") != "online" {
		t.Fatalf("net.transition = %v", sink.names(t))
	}
	if ev := sink.one(t, "net.outage_start"); attr(ev, "diag") != "route=none" || ev.Level != slog.LevelWarn {
		t.Fatalf("net.outage_start = %s", ev.Raw)
	}
	if ev := sink.one(t, "net.recovery_attempt"); attr(ev, "result") != "ok" || attr(ev, "service") != "iPhone USB" {
		t.Fatalf("net.recovery_attempt = %s", ev.Raw)
	}
	end := sink.one(t, "net.outage_end")
	if attr(end, "recoveries") != "1" || attr(end, "duration") == "<missing>" {
		t.Fatalf("net.outage_end = %s", end.Raw)
	}
	// FSM chatter is recorded at Debug only.
	for _, ev := range sink.named(t, "net.note") {
		if ev.Level != slog.LevelDebug {
			t.Fatalf("net.note at %v", ev.Level)
		}
	}
}

func TestEventsLeaseLostAndAcquireFail(t *testing.T) {
	h, sink := newLoggedHarness(t, nil)
	h.start(Options{})
	h.polls(1)
	h.do(func() { h.guard.Active().Lose(errors.New("connection reset")) })
	h.wait()
	if ev := sink.one(t, "lease.lost"); ev.Level != slog.LevelError || !strings.Contains(attr(ev, "err"), "connection reset") {
		t.Fatalf("lease.lost = %s", ev.Raw)
	}
	if attr(sink.one(t, "session.end"), "reason") != string(contracts.EndLeaseLost) {
		t.Fatal("session.end reason")
	}

	h2, sink2 := newLoggedHarness(t, nil)
	h2.guard.FailAcquire(contracts.ErrDaemonUnavailable)
	h2.start(Options{})
	h2.wait()
	got := strings.Join(sink2.names(t), " ")
	if got != "session.start lease.acquire_fail session.end" {
		t.Fatalf("acquire failure events = %s", got)
	}
}

func TestEventsCriticalSleep(t *testing.T) {
	h, sink := newLoggedHarness(t, nil)
	h.start(Options{})
	goCritical(h)
	h.advance(15 * time.Second)
	h.wait()
	p := sink.one(t, "sleep.pending")
	if attr(p, "trigger") != string(contracts.SleepTriggerCritical) || attr(p, "abortable") != "false" {
		t.Fatalf("sleep.pending = %s", p.Raw)
	}
	sink.one(t, "sleep.now")
	sink.one(t, "sleep.request")
	if attr(sink.one(t, "session.end"), "slept") != "true" {
		t.Fatal("session.end slept")
	}
}

// TestEventsOutageTagsDNSChecks: the provider's DNS/IP checks (run only
// when degraded) are tagged on net.outage_start and net.diag.
func TestEventsOutageTagsDNSChecks(t *testing.T) {
	h, sink := newLoggedHarness(t, nil)
	h.start(Options{})
	degraded := func(sys string) contracts.NetSnapshot {
		s := contracts.NetSnapshot{DefaultIface: "en0", Iface: "en10", WiFiIface: "en0", WiFiOn: true,
			Checks: &contracts.NetChecks{DNSSys: sys, DNSDirect: contracts.CheckOK, IPOK: contracts.CheckOK}}
		s.Diag = "route=en0 checks: dns_sys=" + sys
		return s
	}
	h.net.PushSnapshot(degraded(contracts.CheckFail))
	h.polls(2)
	h.net.PushSnapshot(degraded(contracts.CheckOK))
	h.polls(1)
	h.cancel()
	h.wait()

	start := sink.one(t, "net.outage_start")
	if attr(start, "dns_sys") != "fail" || attr(start, "dns_direct") != "ok" || attr(start, "ip_ok") != "ok" ||
		attr(start, "verdict") != "system DNS failing, direct DNS works (VPN/Tailscale DNS?)" {
		t.Fatalf("net.outage_start = %s", start.Raw)
	}
	d := sink.one(t, "net.diag")
	if attr(d, "dns_sys") != "ok" || attr(d, "ip_ok") != "ok" {
		t.Fatalf("net.diag = %s", d.Raw)
	}
	// Probes without checks (online, or an old provider) add no tags.
	if ev := sink.named(t, "net.transition"); len(ev) == 0 || attr(ev[0], "dns_sys") != "<missing>" {
		t.Fatalf("net.transition = %v", ev)
	}
}
