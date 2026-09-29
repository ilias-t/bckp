package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ilias-t/bckp/internal/config"
	"github.com/ilias-t/bckp/internal/contracts"
	"github.com/ilias-t/bckp/internal/fakes"
	"github.com/ilias-t/bckp/internal/paths"
)

// Probes for the Wi-Fi nudge. The phone is never on USB unless noted.

// hotspotWiFi: online via the iPhone hotspot joined over Wi-Fi.
func hotspotWiFi() contracts.NetSnapshot {
	return contracts.NetSnapshot{DefaultIface: "en0", DefaultGateway: contracts.HotspotGateway, WiFiIface: "en0",
		Iface: "en10", Online: true, WiFiOn: true}
}

// otherWiFi: online via another Wi-Fi network (home, café).
func otherWiFi() contracts.NetSnapshot {
	return contracts.NetSnapshot{DefaultIface: "en0", DefaultGateway: "192.0.2.1", WiFiIface: "en0",
		Iface: "en10", Online: true, WiFiOn: true}
}

// noRoute: offline, no default route.
func noRoute(wifiOn bool) contracts.NetSnapshot {
	return contracts.NetSnapshot{WiFiIface: "en0", Iface: "en10", WiFiOn: wifiOn, Diag: "route=none"}
}

// nudgeSeq builds probes: whs (hotspot over Wi-Fi), wo (other Wi-Fi), wn
// (no route, Wi-Fi on), wx (no route, Wi-Fi off), captive (Wi-Fi route,
// no internet), usb (online via iPhone USB), uoff (offline, iPhone on
// USB); "xN" repeats the previous probe.
func nudgeSeq(t *testing.T, spec string) []NetInput {
	t.Helper()
	var out []NetInput
	for _, tok := range strings.Fields(spec) {
		var s contracts.NetSnapshot
		switch {
		case tok == "whs":
			s = hotspotWiFi()
		case tok == "wo":
			s = otherWiFi()
		case tok == "wn":
			s = noRoute(true)
		case tok == "wx":
			s = noRoute(false)
		case tok == "captive":
			s = otherWiFi()
			s.Online = false
		case tok == "usb":
			s = contracts.NetSnapshot{DefaultIface: "en10", DefaultGateway: contracts.HotspotGateway, WiFiIface: "en0", Iface: "en10",
				Online: true, IfacePresent: true, GatewayReachable: true, IPhoneUSBPresent: true, WiFiOn: true}
		case tok == "uoff":
			s = contracts.NetSnapshot{WiFiIface: "en0", Iface: "en10", IfacePresent: true, IPhoneUSBPresent: true, WiFiOn: true}
		case strings.HasPrefix(tok, "x"):
			var n int
			if _, err := fmt.Sscanf(tok, "x%d", &n); err != nil {
				t.Fatalf("bad token %q", tok)
			}
			for i := 1; i < n; i++ {
				out = append(out, out[len(out)-1])
			}
			continue
		default:
			t.Fatalf("bad token %q", tok)
		}
		out = append(out, NetInput{Snap: s})
	}
	return out
}

// runNudge steps r through spec. ActWiFiNudge is answered with NudgeDone
// (nudgeErr) WiFiNudgeGap later, as the engine does. It returns the final
// state and "poll:what" lines for nudges, results, recoveries, Wi-Fi
// changes and notifications, plus every action.
func runNudge(t *testing.T, r networkRules, s NetFSM, spec string, nudgeErr error) (NetFSM, []string, []Action) {
	t.Helper()
	var got []string
	var all []Action
	now := t0
	for i, in := range nudgeSeq(t, spec) {
		var acts []Action
		before := s.NudgeResults
		s, acts = r.Step(s, in, now)
		for _, a := range acts {
			if a.Kind == ActWiFiNudge {
				var more []Action
				s, more = r.NudgeDone(s, nudgeErr, now.Add(WiFiNudgeGap))
				acts = append(acts, more...)
			}
		}
		for _, a := range acts {
			all = append(all, a)
			if d := describe(i+1, a); d != "" {
				got = append(got, d)
			}
		}
		if s.NudgeResults != before {
			got = append(got, fmt.Sprintf("%d:result:%s", i+1, s.Nudge.Result))
		}
		now = now.Add(poll)
	}
	return s, got, all
}

func nudgeRules() networkRules {
	return networkRules{Cfg: config.Default().Network, Poll: poll, Mode: contracts.NetworkPreferHotspot, WiFiNudge: true}
}

func TestWiFiNudgeStep(t *testing.T) {
	tests := []struct {
		name     string
		rules    func(*networkRules)
		spec     string
		nudgeErr error
		want     []string
		nudges   int
	}{
		{
			// Offline from poll 2 (15s); 60s later is poll 6; Wi-Fi back on
			// at 78s; online via the hotspot at poll 8 (105s).
			name:   "offline 60s without USB: nudge, lands on the hotspot",
			spec:   "whs wn x6 whs",
			want:   []string{"6:wifi-nudge", "8:p3:Mac back online after 1m30s via hotspot (Wi-Fi)", "8:result:landed_hotspot"},
			nudges: 1,
		},
		{
			name: "never nudges while online via another Wi-Fi", spec: "wo x40",
		},
		{
			name: "never nudges while online via the hotspot over Wi-Fi", spec: "whs x40",
		},
		{
			name: "never nudges while online via USB", spec: "usb x40",
		},
		{
			// A network that is joined but has no internet is offline.
			name: "a Wi-Fi route without internet counts as offline", spec: "captive x5",
			want: []string{"5:wifi-nudge"}, nudges: 1,
		},
		{
			// The watchdog toggles the USB service instead (after 90s).
			name: "no nudge while the iPhone is on USB", spec: "usb uoff x10",
			want: []string{"8:recover"},
		},
		{
			// Nudges at 60s (#5), 240s (#17), 420s (#29); each is still
			// offline 45s after Wi-Fi is back on.
			name:   "at most once per wifi_nudge_every; still offline",
			spec:   "wn x30",
			want:   []string{"5:wifi-nudge", "9:result:still_offline", "17:wifi-nudge", "21:result:still_offline", "29:wifi-nudge"},
			nudges: 3,
		},
		{
			name:   "online via another network after the nudge",
			spec:   "wn x5 wo",
			want:   []string{"5:wifi-nudge", "6:p3:Mac back online after 1m15s via Wi-Fi", "6:result:other_network"},
			nudges: 1,
		},
		{
			name: "configurable thresholds",
			rules: func(r *networkRules) {
				r.Cfg.WiFiNudgeAfter, r.Cfg.WiFiNudgeEvery = 30*time.Second, 90*time.Second
			},
			spec: "wn x10", want: []string{"3:wifi-nudge", "7:result:still_offline", "9:wifi-nudge"}, nudges: 2,
		},
		{
			name: "wifi_nudge_after = 0 disables", rules: func(r *networkRules) { r.Cfg.WiFiNudgeAfter = 0 },
			spec: "wn x30",
		},
		{
			// Mode alone says prefer_hotspot after auto engaged; the flag
			// is what separates "on" from "auto".
			name: "auto (WiFiNudge off) never nudges", rules: func(r *networkRules) { r.WiFiNudge = false },
			spec: "wn x30",
		},
		{
			name: "a failed toggle is a result, and still rate limited",
			spec: "wn x20", nudgeErr: errors.New("networksetup: error"),
			want:   []string{"5:wifi-nudge", "5:result:failed", "17:wifi-nudge", "17:result:failed"},
			nudges: 2,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := nudgeRules()
			if tc.rules != nil {
				tc.rules(&r)
			}
			s, got, _ := runNudge(t, r, NetFSM{}, tc.spec, tc.nudgeErr)
			if strings.Join(got, "\n") != strings.Join(tc.want, "\n") {
				t.Errorf("actions:\n got %q\nwant %q", got, tc.want)
			}
			if s.Nudges != tc.nudges {
				t.Errorf("nudges = %d, want %d", s.Nudges, tc.nudges)
			}
		})
	}
}

// TestWiFiNudgeNeverWhileOnline is the regression guard: whatever the
// timing, a probe that is online never produces a nudge, even with an old
// outage behind it and every threshold at its minimum.
func TestWiFiNudgeNeverWhileOnline(t *testing.T) {
	r := nudgeRules()
	r.Cfg.WiFiNudgeAfter, r.Cfg.WiFiNudgeEvery = time.Nanosecond, time.Nanosecond
	for _, spec := range []string{"wo x50", "wn x2 wo x50", "whs x50", "wn x10 whs x50", "usb x50"} {
		s := NetFSM{}
		now := t0
		for i, in := range nudgeSeq(t, spec) {
			var step []Action
			s, step = r.Step(s, in, now)
			if in.Snap.Online && slices.ContainsFunc(step, func(a Action) bool { return a.Kind == ActWiFiNudge }) {
				t.Fatalf("%s: nudged at poll %d while online", spec, i+1)
			}
			if s.NudgeInFlight {
				s, _ = r.NudgeDone(s, nil, now)
			}
			now = now.Add(poll)
		}
	}
}

func TestWiFiNudgeNotesOtherWiFiOnce(t *testing.T) {
	_, _, acts := runNudge(t, nudgeRules(), NetFSM{}, "wo x10 wn wo x10", nil)
	n := 0
	for _, a := range acts {
		if a.Kind == ActLog && strings.Contains(a.Msg, "non-hotspot Wi-Fi") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("non-hotspot Wi-Fi noted %d times, want once per session", n)
	}
	r := nudgeRules()
	r.WiFiNudge = false
	_, _, acts = runNudge(t, r, NetFSM{}, "wo x3", nil)
	for _, a := range acts {
		if strings.Contains(a.Msg, "non-hotspot") {
			t.Fatal("noted without the nudge enabled")
		}
	}
}

// TestWiFiNudgeLandWindow: a result is decided on the first online probe,
// and still_offline only once WiFiNudgeLandWindow (45s) has passed.
func TestWiFiNudgeLandWindow(t *testing.T) {
	r := nudgeRules()
	// Nudge at poll 5 (60s), Wi-Fi on at 63s; polls at 75s, 90s and 105s
	// are within 45s; landing at 105s (42s after on) is still landed.
	s, got, _ := runNudge(t, r, NetFSM{}, "wn x7 whs", nil)
	if want := "8:result:landed_hotspot"; !slices.Contains(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
	if s.Nudge.After != 42*time.Second || s.Nudge.Link != contracts.LinkHotspotWiFi {
		t.Fatalf("landed after %s via %s", s.Nudge.After, s.Nudge.Link)
	}
}

func TestWiFiNudgeWhenWiFiOff(t *testing.T) {
	// Wi-Fi off and the phone gone: the nudge only turns Wi-Fi on.
	s, got, _ := runNudge(t, nudgeRules(), NetFSM{Probed: true, Online: true}, "wx x6", nil)
	if want := []string{"5:wifi-nudge"}; !slices.Equal(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
	if !s.Nudge.PowerOnOnly {
		t.Fatalf("state after a power-on nudge: %+v", s)
	}
}

func TestWiFiNudgeBackOnlineBody(t *testing.T) {
	_, _, acts := runNudge(t, nudgeRules(), NetFSM{}, "whs wn x6 whs", nil)
	var body string
	for _, a := range acts {
		if a.Kind == ActNotify {
			body = a.Notification.Body
		}
	}
	for _, want := range []string{
		"12:01 nudge: Wi-Fi off/on (offline 1m)",
		"12:01 nudge: landed on the hotspot (Wi-Fi) after 27s",
		"Recovery attempts: 0",
		"Wi-Fi nudges: 1",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q:\n%s", want, body)
		}
	}
}

// ---- engine: two-phase toggle, journal, auto vs on ----

// nudgeHarness is a hotspot "on" session whose first probe is online via
// the hotspot over Wi-Fi, then offline with no route (phone not on USB):
// the nudge starts at the poll at 75s and turns Wi-Fi on at 78s.
func nudgeHarness(t *testing.T, mutate func(*config.Config)) *harness {
	h := newHarness(t, func(c *config.Config) {
		c.Network.PreferHotspot = config.HotspotOn
		if mutate != nil {
			mutate(c)
		}
	})
	h.net = fakes.NewNetwork(h.clock, testOrder...)
	h.deps.Network = h.net
	h.net.PushSnapshot(hotspotWiFi())
	h.net.PushSnapshot(noRoute(true))
	return h
}

func (h *harness) wifiCalls() []string {
	var out []string
	for _, c := range h.net.Calls() {
		if strings.HasPrefix(c, fakes.MethodSetWiFiPower) {
			out = append(out, c)
		}
	}
	return out
}

func (h *harness) journal() *Journal {
	h.t.Helper()
	j, err := readJournal(h.paths.RestoreJournal)
	if err != nil {
		h.t.Fatal(err)
	}
	return j
}

var (
	nudgeAt   = t0.Add(75 * time.Second)
	nudgeOnAt = nudgeAt.Add(WiFiNudgeGap)
)

func TestWiFiNudgeTwoPhase(t *testing.T) {
	h := nudgeHarness(t, nil)
	h.start(Options{})
	h.advanceTo(nudgeAt.Add(-time.Second))
	if len(h.wifiCalls()) != 0 {
		t.Fatalf("nudged before 60s offline: %v", h.wifiCalls())
	}
	h.advanceTo(nudgeAt)
	if got := h.wifiCalls(); !slices.Equal(got, []string{"SetWiFiPower false"}) || h.net.WiFiOn() {
		t.Fatalf("off phase: %v", got)
	}
	if j := h.journal(); j == nil || !j.WiFiNudge || !j.WiFiChanged || j.WiFiPower == nil || !*j.WiFiPower {
		t.Fatalf("the off phase must be journaled first: %+v", j)
	}
	// Commands are answered between the phases.
	h.do(func() {
		if _, err := h.e.Extend(context.Background(), 30, false); err != nil {
			t.Error(err)
		}
	})
	if h.net.WiFiOn() {
		t.Fatal("Wi-Fi came back before the gap")
	}
	h.advanceTo(nudgeOnAt)
	if got := h.wifiCalls(); !slices.Equal(got, []string{"SetWiFiPower false", "SetWiFiPower true"}) || !h.net.WiFiOn() {
		t.Fatalf("on phase: %v", got)
	}
	if j := h.journal(); j.WiFiNudge || !j.WiFiChanged {
		t.Fatalf("journal after the on phase: %+v", j)
	}
	snap := h.e.Snapshot()
	if snap.Network.WiFiNudges != 1 || !snap.Network.LastWiFiNudge.Equal(nudgeAt) || snap.Network.LastWiFiNudgeResult != "" {
		t.Fatalf("snapshot network: %+v", snap.Network)
	}

	// macOS joins the hotspot; the next poll (90s) sees it, and "back
	// online" goes out one poll later.
	h.net.PushSnapshot(hotspotWiFi())
	h.advanceTo(t0.Add(105 * time.Second))
	if !h.notified("Mac back online after 1m15s via hotspot (Wi-Fi)") || !h.notifiedBody("Wi-Fi nudges: 1") {
		t.Fatalf("back-online notification: %q", h.bodies())
	}
	snap = h.e.Snapshot()
	if snap.Network.LastWiFiNudgeResult != NudgeLandedHotspot || snap.Network.Link != contracts.LinkHotspotWiFi {
		t.Fatalf("snapshot network: %+v", snap.Network)
	}
	// No notification per nudge: start and back online only.
	for _, title := range h.titles() {
		if strings.Contains(strings.ToLower(title), "nudge") {
			t.Fatalf("a nudge was notified: %q", h.titles())
		}
	}

	h.do(func() { _ = h.e.Stop(context.Background(), nil) })
	h.wait()
	if h.journalExists() || !h.net.WiFiOn() {
		t.Fatalf("exit: journal %v, wifi %v", h.journalExists(), h.net.WiFiOn())
	}
}

func TestWiFiNudgeStopMidToggleRestoresWiFi(t *testing.T) {
	h := nudgeHarness(t, nil)
	h.start(Options{})
	h.advanceTo(nudgeAt)
	if h.net.WiFiOn() {
		t.Fatal("expected the off phase")
	}
	h.do(func() { _ = h.e.Stop(context.Background(), nil) })
	h.wait()
	if !h.net.WiFiOn() || h.journalExists() {
		t.Fatalf("stop mid-toggle: wifi %v, journal %v, calls %v", h.net.WiFiOn(), h.journalExists(), h.wifiCalls())
	}
}

func TestWiFiNudgeOffPhaseFailure(t *testing.T) {
	h := nudgeHarness(t, nil)
	h.start(Options{})
	h.advanceTo(nudgeAt.Add(-time.Second))
	h.net.FailNext(fakes.MethodSetWiFiPower, errors.New("networksetup: error"))
	h.advanceTo(nudgeAt)
	// Turning off failed: Wi-Fi is turned on right away (never left off).
	if got := h.wifiCalls(); !slices.Equal(got, []string{"SetWiFiPower false", "SetWiFiPower true"}) || !h.net.WiFiOn() {
		t.Fatalf("calls %v", got)
	}
	if snap := h.e.Snapshot(); snap.Network.LastWiFiNudgeResult != NudgeFailed {
		t.Fatalf("result %q", snap.Network.LastWiFiNudgeResult)
	}
	if j := h.journal(); j.WiFiNudge {
		t.Fatal("journal still flags a nudge in progress")
	}
}

func TestWiFiNudgeOnPhaseRetries(t *testing.T) {
	h := nudgeHarness(t, nil)
	h.start(Options{})
	h.advanceTo(nudgeAt)
	h.net.FailNext(fakes.MethodSetWiFiPower, errors.New("networksetup: error"))
	h.advanceTo(nudgeOnAt)
	if h.net.WiFiOn() || h.e.Snapshot().Errors["network"] == "" {
		t.Fatal("expected the on phase to fail and be reported")
	}
	if j := h.journal(); !j.WiFiNudge {
		t.Fatal("the journal must keep the nudge flagged until Wi-Fi is on")
	}
	// With Wi-Fi stuck off the Mac may have no way online: that needs
	// attention now, not after three failed attempts.
	if !h.notified("Wi-Fi stuck off after a nudge") {
		t.Fatalf("no alert: %q", h.notifTitles())
	}
	// Retried one poll later; the 90s probe in between starts no new nudge.
	h.advanceTo(nudgeOnAt.Add(h.cfg.Session.Poll))
	if !h.net.WiFiOn() || h.e.Snapshot().Errors["network"] != "" {
		t.Fatalf("retry: wifi %v, errors %v", h.net.WiFiOn(), h.e.Snapshot().Errors)
	}
	if got := h.wifiCalls(); len(got) != 3 {
		t.Fatalf("calls %v", got)
	}
}

func TestWiFiNudgeCrashMidToggleRestoredFromJournal(t *testing.T) {
	ctx := context.Background()
	p := paths.ForHome(t.TempDir())
	n := fakes.NewNetwork(nil, "Wi-Fi", "iPhone USB")
	_ = n.SetWiFiPower(ctx, false)
	on := true
	// A crash between the off and on phases leaves this journal behind.
	if err := writeJournal(p.RestoreJournal, Journal{Version: JournalVersion, Service: "iPhone USB",
		ServiceOrder: []string{"Wi-Fi", "iPhone USB"}, WiFiPower: &on, WiFiChanged: true, WiFiNudge: true}); err != nil {
		t.Fatal(err)
	}
	if restored, err := RestoreStaleJournal(ctx, n, p); !restored || err != nil {
		t.Fatalf("restore: %v %v", restored, err)
	}
	if !n.WiFiOn() {
		t.Fatal("Wi-Fi not restored after a crash mid-nudge")
	}

	// The flag alone, with the original power unknown, turns Wi-Fi on.
	_ = n.SetWiFiPower(ctx, false)
	if err := applyJournal(ctx, n, Journal{Version: JournalVersion, WiFiNudge: true}); err != nil || !n.WiFiOn() {
		t.Fatalf("flag only: %v, wifi %v", err, n.WiFiOn())
	}
	// The original power wins: Wi-Fi was off before the session.
	off := false
	if err := applyJournal(ctx, n, Journal{Version: JournalVersion, WiFiPower: &off, WiFiChanged: true, WiFiNudge: true}); err != nil || n.WiFiOn() {
		t.Fatalf("original off: %v, wifi %v", err, n.WiFiOn())
	}
}

// TestWiFiNudgeAutoNeverNudges goes through auto's switch-over, which
// makes the mode prefer_hotspot, then loses the phone: still no nudge.
func TestWiFiNudgeAutoNeverNudges(t *testing.T) {
	h := autoHarness(t, nil)
	h.net = fakes.NewNetwork(h.clock, testOrder...)
	h.deps.Network = h.net
	usb := online()
	usb.IPhoneUSBPresent, usb.WiFiIface = true, "en0"
	h.net.PushSnapshot(usb)
	h.net.PushSnapshot(noRoute(true))
	h.start(Options{})
	h.polls(40)
	if h.e.Snapshot().Network.Mode != contracts.NetworkPreferHotspot {
		t.Fatalf("auto did not engage: %s", h.e.Snapshot().Network.Mode)
	}
	if got := h.wifiCalls(); len(got) != 0 {
		t.Fatalf("auto nudged Wi-Fi: %v", got)
	}
	h.do(func() { _ = h.e.Stop(context.Background(), nil) })
	h.wait()
}

func TestWiFiNudgeOnNeverWhileOnlineElsewhere(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.Network.PreferHotspot = config.HotspotOn })
	h.net = fakes.NewNetwork(h.clock, testOrder...)
	h.deps.Network = h.net
	h.net.PushSnapshot(otherWiFi())
	h.start(Options{})
	h.polls(60)
	if got := h.wifiCalls(); len(got) != 0 {
		t.Fatalf("nudged while online on another network: %v", got)
	}
	h.do(func() { _ = h.e.Stop(context.Background(), nil) })
	h.wait()
}

// TestWiFiNudgeNeedsJournaledPower: without the original Wi-Fi power the
// nudge is off for the session.
func TestWiFiNudgeNeedsJournaledPower(t *testing.T) {
	h := nudgeHarness(t, nil)
	h.net.FailNext(fakes.MethodWiFiPower, errors.New("no wifi"))
	h.start(Options{})
	h.polls(30)
	if got := h.wifiCalls(); len(got) != 0 {
		t.Fatalf("nudged without the original power journaled: %v", got)
	}
	h.do(func() { _ = h.e.Stop(context.Background(), nil) })
	h.wait()
}

func TestWiFiNudgeEvents(t *testing.T) {
	h, sink := newLoggedHarness(t, func(c *config.Config) { c.Network.PreferHotspot = config.HotspotOn })
	h.net = fakes.NewNetwork(h.clock, testOrder...)
	h.deps.Network = h.net
	h.net.PushSnapshot(otherWiFi())
	h.net.PushSnapshot(noRoute(true))
	h.start(Options{})
	h.advanceTo(nudgeOnAt)
	h.net.PushSnapshot(hotspotWiFi())
	h.advanceTo(t0.Add(90 * time.Second))
	h.do(func() { _ = h.e.Stop(context.Background(), nil) })
	h.wait()

	ev := sink.one(t, "net.wifi_nudge")
	if attr(ev, "result") != NudgeLandedHotspot || attr(ev, "landed_after") != "12s" || attr(ev, "offline_for") != "1m0s" ||
		attr(ev, "reason") != "offline 1m" || attr(ev, "link") != "hotspot (Wi-Fi)" || attr(ev, "toggle") != "3s" {
		t.Fatalf("net.wifi_nudge = %s", ev.Raw)
	}
	var links []string
	for _, tr := range sink.named(t, "net.transition") {
		links = append(links, attr(tr, "from")+"/"+attr(tr, "from_link")+">"+attr(tr, "to")+"/"+attr(tr, "link"))
	}
	if want := []string{"online/Wi-Fi>offline/none", "offline/none>online/hotspot (Wi-Fi)"}; !slices.Equal(links, want) {
		t.Fatalf("transitions %q, want %q", links, want)
	}
	samples := sink.named(t, "sample")
	if attr(samples[0], "link") != "Wi-Fi" {
		t.Fatalf("sample = %s", samples[0].Raw)
	}
}

func TestWiFiNudgeLinkChangeWhileOnline(t *testing.T) {
	h, sink := newLoggedHarness(t, nil)
	h.net = fakes.NewNetwork(h.clock, testOrder...)
	h.deps.Network = h.net
	h.net.PushSnapshot(otherWiFi())
	h.net.PushSnapshot(hotspotWiFi())
	h.start(Options{})
	h.polls(2)
	h.do(func() { _ = h.e.Stop(context.Background(), nil) })
	h.wait()
	tr := sink.named(t, "net.transition")
	if len(tr) != 1 || attr(tr[0], "from") != "online" || attr(tr[0], "to") != "online" ||
		attr(tr[0], "from_link") != "Wi-Fi" || attr(tr[0], "link") != "hotspot (Wi-Fi)" {
		t.Fatalf("transitions: %v", tr)
	}
}
