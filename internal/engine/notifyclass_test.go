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

func stopWith(h *harness, sleep *bool) {
	h.do(func() { _ = h.e.Stop(context.Background(), sleep) })
	h.wait()
}

const (
	info = "info: "
	attn = "attention: "
)

// classScenarios run the engine through every notification it can send.
// want lists each scenario's notifications in order, as "class: title".
// "Mac back online" depends on the outage; TestBackOnlineClass covers it.
var classScenarios = []struct {
	name string
	run  func(t *testing.T) *harness
	want []string
}{
	{"session and fair", func(t *testing.T) *harness {
		h := newHarness(t, func(c *config.Config) { c.Notify.Heartbeat = time.Minute })
		h.pushThermal(contracts.ThermalFair, 2)
		h.pushThermal(contracts.ThermalNominal, 3)
		h.start(Options{})
		h.polls(6)
		h.do(func() { _, _ = h.e.Extend(context.Background(), 30, false) })
		stopWith(h, nil)
		return h
	}, []string{info + "backpack on (forever)", info + "Mac warming up (fair)", info + "Mac: nominal, battery 30°C",
		info + "Mac back to nominal", info + "backpack extended (forever session)", info + "backpack off"}},
	{"hot, abort, critical, sleep failed", func(t *testing.T) *harness {
		h := newHarness(t, nil)
		h.guard.SetSleepErr(errors.New("daemon gone"))
		h.pushThermal(contracts.ThermalSerious, 1)
		h.start(Options{})
		h.polls(41)
		h.do(func() { _, _ = h.e.AbortSleep(context.Background()) })
		h.pushThermal(contracts.ThermalCritical, 2)
		h.polls(3)
		h.advance(time.Minute)
		h.wait()
		return h
	}, []string{info + "backpack on (forever)", attn + "Mac is HOT (serious) - open the bag", attn + "Sleeping in 1m: hot for 10m",
		info + "Thermal sleep cancelled", attn + "Mac CRITICAL heat - take it out now",
		attn + "Sleeping in 15s: critical heat for 2 polls", attn + "Mac sleeping now (thermal)", attn + "Sleep failed - lease released"}},
	{"cooled down", func(t *testing.T) *harness {
		h := newHarness(t, nil)
		h.pushThermal(contracts.ThermalSerious, 1)
		h.pushThermal(contracts.ThermalNominal, 2)
		h.start(Options{})
		h.polls(3)
		stopWith(h, nil)
		return h
	}, []string{info + "backpack on (forever)", attn + "Mac is HOT (serious) - open the bag", attn + "Mac cooled down (nominal)",
		info + "backpack off"}},
	{"readings failing and back, power", func(t *testing.T) *harness {
		h := newHarness(t, nil)
		for range 4 {
			h.thermal.PushErr(errors.New("no thermal"))
			h.power.PushErr(errors.New("no power"))
		}
		h.pushThermal(contracts.ThermalNominal, 1)
		h.power.Push(contracts.PowerReading{OnAC: true, Percent: 90})
		h.power.Push(contracts.PowerReading{OnAC: false, Percent: 80})
		h.power.Push(contracts.PowerReading{OnAC: true, Percent: 80})
		h.power.Push(contracts.PowerReading{OnAC: false, Percent: 20})
		h.start(Options{})
		h.polls(10)
		h.wait()
		return h
	}, []string{info + "backpack on (forever)", attn + "Thermal readings failing", attn + "Power readings failing",
		info + "Thermal readings back", attn + "On battery (80%)", info + "Back on AC (80%)", attn + "On battery (20%)",
		attn + "Battery 20% - backpack off"}},
	{"expired", func(t *testing.T) *harness {
		h := newHarness(t, nil)
		h.start(Options{Duration: time.Minute})
		h.advance(2 * time.Minute)
		h.wait()
		return h
	}, []string{info + "backpack on until 12:01", info + "backpack off: time's up"}},
	{"cap warning and cap", func(t *testing.T) *harness {
		h := newHarness(t, func(c *config.Config) { c.Session.MaxDuration = 20 * time.Minute })
		h.start(Options{})
		h.advance(21 * time.Minute)
		h.wait()
		return h
	}, []string{info + "backpack on (forever)", attn + "backpack ends in 10m (20m cap)", attn + "backpack off: 20m cap reached"}},
	{"expiry on battery", func(t *testing.T) *harness {
		h := newHarness(t, nil)
		h.power.Push(contracts.PowerReading{OnAC: false, Percent: 60})
		h.start(Options{Duration: time.Minute})
		h.advance(2 * time.Minute)
		h.wait()
		return h
	}, []string{info + "backpack on until 12:01", attn + "On battery (60%)", attn + "backpack off: time's up (on battery, 60%)"}},
	{"stop --sleep", func(t *testing.T) *harness {
		h := newHarness(t, nil)
		h.start(Options{})
		stopWith(h, ptr(true))
		return h
	}, []string{info + "backpack on (forever)", info + "backpack off - sleeping Mac"}},
	{"SIGTERM", func(t *testing.T) *harness {
		h := newHarness(t, nil)
		h.start(Options{})
		h.do(func() { _ = h.e.StopForSignal(context.Background(), "SIGTERM") })
		h.wait()
		return h
	}, []string{info + "backpack on (forever)", attn + "backpack off (signal)"}},
	{"session ctx cancelled", func(t *testing.T) *harness {
		h := newHarness(t, nil)
		h.start(Options{})
		h.cancel()
		h.wait()
		return h
	}, []string{info + "backpack on (forever)", attn + "backpack off (signal)"}},
	{"lease lost", func(t *testing.T) *harness {
		h := newHarness(t, nil)
		h.start(Options{})
		h.do(func() { h.guard.Active().Lose(errors.New("connection reset")) })
		h.wait()
		return h
	}, []string{info + "backpack on (forever)", attn + "Sleep guard lost - backpack off"}},
	{"engine error", func(t *testing.T) *harness {
		h := newHarness(t, nil)
		h.deps.Thermal = &panicThermal{n: 2}
		h.start(Options{})
		h.polls(1)
		h.wait()
		return h
	}, []string{info + "backpack on (forever)", attn + "backpack stopped: engine error"}},
	{"hotspot preference off", func(t *testing.T) *harness {
		h := newHarness(t, func(c *config.Config) {
			c.Network.PreferHotspot = config.HotspotOn
			c.Network.Service = "No Such Service"
		})
		h.start(Options{})
		stopWith(h, nil)
		return h
	}, []string{attn + "Hotspot preference off", info + "backpack on (forever)", info + "backpack off"}},
	{"restore failed", func(t *testing.T) *harness {
		h := newHarness(t, func(c *config.Config) { c.Network.PreferHotspot = config.HotspotOn })
		h.start(Options{})
		h.net.FailNext(fakes.MethodServiceOrder, errors.New("networksetup: error"))
		stopWith(h, nil)
		return h
	}, []string{info + "backpack on (forever)", attn + "Network restore failed", info + "backpack off"}},
	{"still offline", func(t *testing.T) *harness {
		h := nudgeHarness(t, nil)
		h.start(Options{})
		h.advanceTo(t0.Add(10 * time.Minute))
		h.net.PushSnapshot(hotspotWiFi())
		h.polls(1)
		stopWith(h, nil)
		return h
	}, []string{info + "backpack on (forever)", attn + "Still offline after 3 recovery attempts",
		attn + "Mac back online after 10m via hotspot (Wi-Fi)", info + "backpack off"}},
	{"Wi-Fi stuck off", func(t *testing.T) *harness {
		h := nudgeHarness(t, nil)
		h.start(Options{})
		h.advanceTo(nudgeAt)
		h.net.FailNext(fakes.MethodSetWiFiPower, errors.New("networksetup: error"))
		h.advanceTo(nudgeOnAt)
		stopWith(h, nil)
		return h
	}, []string{info + "backpack on (forever)", attn + "Wi-Fi stuck off after a nudge", info + "backpack off"}},
}

func TestNotificationClasses(t *testing.T) {
	for _, sc := range classScenarios {
		t.Run(sc.name, func(t *testing.T) {
			h := sc.run(t)
			var got []string
			for _, n := range h.notifs() {
				got = append(got, string(n.Class)+": "+n.Title)
			}
			if !slices.Equal(got, sc.want) {
				t.Errorf("notifications:\n got %q\nwant %q", got, sc.want)
			}
		})
	}
}

// TestBackOnlineClass: "back online" needs attention only after a long
// outage (notify.outage_min) or one that raised "still offline".
func TestBackOnlineClass(t *testing.T) {
	r := networkRules{Cfg: config.Default().Network, Poll: poll, Mode: contracts.NetworkWatchdog, OutageMin: 5 * time.Minute}
	classOf := func(t *testing.T, r networkRules, spec string) (contracts.NotifyClass, []string) {
		t.Helper()
		_, _, acts := runNet(t, r, NetFSM{}, spec, false, t0, nil)
		var class contracts.NotifyClass
		var titles []string
		for _, a := range acts {
			if a.Kind == ActNotify {
				titles = append(titles, a.Notification.Title)
				if strings.HasPrefix(a.Notification.Title, "Mac back online") {
					class = a.Notification.Class
				}
			}
		}
		return class, titles
	}
	if c, _ := classOf(t, r, "on off x5 on"); c != contracts.ClassInfo {
		t.Errorf("1m15s outage: %q, want info", c)
	}
	// Offline 15s..5m15s (21 polls): exactly outage_min.
	if c, _ := classOf(t, r, "on off x20 on"); c != contracts.ClassAttention {
		t.Errorf("5m outage: %q, want attention", c)
	}
	// Recoveries at 1m45s, 4m45s and 7m45s; "still offline" 45s after the
	// third; back online right after, under a (raised) outage_min.
	r.OutageMin = time.Hour
	c, titles := classOf(t, r, "on off x35 on")
	if c != contracts.ClassAttention || !slices.Contains(titles, "Still offline after 3 recovery attempts") {
		t.Errorf("after a failing alert: %q (%q), want attention", c, titles)
	}
	if n := strings.Count(strings.Join(titles, "\n"), "Still offline"); n != 1 {
		t.Errorf("failing alert sent %d times, want once per outage", n)
	}
}

// TestPushPolicy: with notify.push = "attention" (the default) info
// notifications reach the log and the snapshot but not the notifier; with
// "all" everything is pushed.
func TestPushPolicy(t *testing.T) {
	h, sink := newLoggedHarness(t, func(c *config.Config) { c.Notify.Push = config.PushAttention })
	h.pushThermal(contracts.ThermalSerious, 1)
	h.start(Options{})
	h.polls(1)
	stopWith(h, nil)

	if got := h.titles(); !slices.Equal(got, []string{"Mac is HOT (serious) - open the bag"}) {
		t.Fatalf("pushed %q, want only the attention one", got)
	}
	logged := sink.named(t, "notify.logged")
	if len(logged) != 2 || attr(logged[0], "title") != "backpack on (forever)" || attr(logged[0], "pushed") != "false" ||
		attr(logged[0], "class") != "info" || !strings.Contains(attr(logged[0], "body"), "Hotspot") {
		t.Fatalf("notify.logged = %v", logged)
	}
	if strings.Contains(sink.raw(), secretTopic) {
		t.Fatal("topic leaked")
	}
	sent := sink.one(t, "notify.sent")
	if attr(sent, "class") != "attention" || attr(sent, "pushed") != "true" {
		t.Fatalf("notify.sent = %s", sent.Raw)
	}
	var statuses []string
	for _, r := range h.e.Snapshot().Notifications {
		statuses = append(statuses, r.Notification.Title+":"+string(r.Status))
	}
	want := []string{"backpack on (forever):logged", "Mac is HOT (serious) - open the bag:sent", "backpack off:logged"}
	if !slices.Equal(statuses, want) {
		t.Fatalf("recent = %q, want %q", statuses, want)
	}

	h = newHarness(t, func(c *config.Config) { c.Notify.Push = config.PushAll })
	h.start(Options{})
	stopWith(h, nil)
	if got := h.titles(); !slices.Equal(got, []string{"backpack on (forever)", "backpack off"}) {
		t.Fatalf("push all: %q", got)
	}
}
