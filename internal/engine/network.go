package engine

import (
	"fmt"
	"strings"
	"time"

	"github.com/ilias-t/bckp/internal/config"
	"github.com/ilias-t/bckp/internal/contracts"
	"github.com/ilias-t/bckp/internal/humanize"
)

// Network watchdog:
//
//   - Offline means no default route or !Online. A probe error is neither
//     online nor offline: nothing changes.
//   - During an outage the first and the latest diagnostics are kept, plus
//     notes (recovery attempts, sleep gaps).
//   - After RecoveryAfter offline with the hotspot interface present (and
//     Watchdog enabled), ActRecover asks the engine to toggle the service
//     off, wait 3s, and on again; at most once per RecoveryMinInterval.
//     RecoveryDone reports the toggle back and counts it.
//   - When the link returns, a "back online after Xm" notification (p3)
//     carries the outage window, diagnostics, notes and attempt count.
//   - A gap between polls longer than 3 poll intervals is logged as
//     "probably slept" and noted in the outage.
//
// With prefer_hotspot = "auto" (NetworkHotspotAuto), every successful
// probe that finds the hotspot connected (hotspotConnected) asks for
// ActPreferHotspot; the engine then reorders the services and switches the
// mode to NetworkPreferHotspot, so it is asked at most once per session.
// Unplugging the phone later leaves the order as it is: macOS skips a
// service without a link, so Wi-Fi carries the traffic, and re-plugging
// needs no new change. The watchdog (when on) skips recovery while the
// interface is missing.
//
// Wi-Fi nudge (networkRules.WiFiNudge: prefer_hotspot = "on" only, never
// "auto"). backpack cannot join the iPhone hotspot over Wi-Fi by name
// (SSIDs are redacted for command-line tools), so it nudges macOS's own
// auto-join instead ("Ask to join hotspots: Automatic" can wake Instant
// Hotspot over Bluetooth):
//
//   - When offline for WiFiNudgeAfter with the iPhone not on USB, ActWiFiNudge
//     asks the engine to power-cycle Wi-Fi (off, WiFiNudgeGap, on), or just
//     to turn it on when it is off; at most once per WiFiNudgeEvery.
//   - NudgeDone reports Wi-Fi back on. The next probes classify the
//     result: online via the hotspot over Wi-Fi (gateway 172.20.10.1) is
//     landed_hotspot, online via anything else is other_network, and still
//     offline WiFiNudgeLandWindow later is still_offline.
//   - Never while online: a working network (home Wi-Fi, a café) would be
//     dropped, and macOS would pick it again anyway. Online via non-hotspot
//     Wi-Fi is noted once per session instead.

// recoveryFailAttempts USB toggles and Wi-Fi nudges (together) in one
// outage without getting back online raise a "still offline" alert.
const recoveryFailAttempts = 3

// Wi-Fi nudge timings that are not user-configurable.
const (
	// WiFiNudgeGap is the pause between Wi-Fi off and on in a nudge.
	WiFiNudgeGap = 3 * time.Second
	// WiFiNudgeLandWindow is how long after Wi-Fi is back on a nudge may
	// take to land before it counts as still_offline. Measured: macOS
	// joined the hotspot about 19s after Wi-Fi came back on (the phone
	// locked in a pocket, woken over Bluetooth).
	WiFiNudgeLandWindow = 45 * time.Second
)

// Wi-Fi nudge results (nudgeRecord.Result, event net.wifi_nudge).
const (
	NudgeLandedHotspot = "landed_hotspot"
	NudgeOtherNetwork  = "other_network"
	NudgeStillOffline  = "still_offline"
	NudgeFailed        = "failed"
)

// nudgeRecord is one Wi-Fi nudge.
type nudgeRecord struct {
	// Attempt numbers the nudge in the session (1-based).
	Attempt int
	// Reason is why it ran ("offline 1m").
	Reason string
	// PowerOnOnly: Wi-Fi was off, so it was only turned on.
	PowerOnOnly bool
	// Started is when the nudge began; OnAt when Wi-Fi was back on.
	Started, OnAt time.Time
	// OfflineFor is the outage length when the nudge began.
	OfflineFor time.Duration
	// Result is one of the Nudge* results ("" while pending).
	Result string
	// Link is the link at the result.
	Link contracts.NetLink
	// After is from OnAt to the probe that decided the result.
	After time.Duration
	// Err is the failure (Result NudgeFailed).
	Err string
}

// networkRules is the network watchdog's configuration.
type networkRules struct {
	Cfg  config.Network
	Poll time.Duration
	Mode contracts.NetworkMode
	// WiFiNudge enables the Wi-Fi nudge: the session's hotspot preference
	// is "on" (not "auto": Mode alone cannot tell, since auto becomes
	// NetworkPreferHotspot once engaged) and the original Wi-Fi power is
	// journaled. Cfg.WiFiNudgeAfter = 0 disables it too.
	WiFiNudge bool
	// OutageMin (config notify.outage_min) is how long an outage must last
	// for "back online" to need attention; shorter ones are info. A
	// "recovery failing" alert in the outage makes it attention anyway.
	OutageMin time.Duration
}

// NetFSM is the network watchdog's state. The zero value is the initial
// state (online assumed until the first probe says otherwise).
type NetFSM struct {
	// Probed is true after the first successful probe.
	Probed bool
	Online bool
	// OfflineSince is when the current outage began (zero when online).
	OfflineSince time.Time
	// First and Latest are the outage's first and latest diagnostics.
	First, Latest string
	// Notes are outage events (recovery attempts, sleep gaps).
	Notes []string
	// OutageRecoveries counts toggles run in the current outage.
	OutageRecoveries int
	// SkipNoted: "recovery skipped, interface missing" was logged in this
	// outage.
	SkipNoted bool

	// LastRecovery is when the latest toggle was asked for.
	LastRecovery time.Time
	// Recoveries counts toggles run this session.
	Recoveries int
	LastPoll   time.Time
	FailCount  int
	// Snap is the latest successful probe.
	Snap *contracts.NetSnapshot

	// Wi-Fi nudge. NudgeInFlight is set from ActWiFiNudge until NudgeDone;
	// NudgeOnAt (Wi-Fi back on) is non-zero while the result is pending.
	NudgeInFlight bool
	NudgeOnAt     time.Time
	LastNudge     time.Time
	// Nudges counts nudges this session, OutageNudges in this outage.
	Nudges, OutageNudges int
	// Nudge is the latest nudge; NudgeResults counts decided results, so
	// a change means Nudge.Result was just set.
	Nudge        nudgeRecord
	NudgeResults int
	// OtherWiFiNoted: "online via non-hotspot Wi-Fi" was logged this
	// session.
	OtherWiFiNoted bool
	// FailingNotified: "still offline after N recovery attempts" was sent
	// in this outage.
	FailingNotified bool
}

// NetInput is one network probe.
type NetInput struct {
	Snap contracts.NetSnapshot
	Err  error
}

func (r networkRules) nudgeEnabled() bool {
	return r.WiFiNudge && r.Cfg.WiFiNudgeAfter > 0 && r.Mode != contracts.NetworkOff
}

// Step advances the watchdog by one probe. It is pure.
func (r networkRules) Step(s NetFSM, in NetInput, now time.Time) (NetFSM, []Action) {
	s = s.clone()
	var acts []Action
	if !s.LastPoll.IsZero() && r.Poll > 0 {
		if gap := now.Sub(s.LastPoll); gap > 3*r.Poll {
			acts = append(acts, logf("resumed after %s without polling (Mac asleep?)", humanize.Duration(gap)))
			if !s.OfflineSince.IsZero() {
				s.Notes = append(s.Notes, fmt.Sprintf("%s note: no polls for %s (Mac asleep?)", humanize.Clock(now, now), humanize.Duration(gap)))
			}
		}
	}
	s.LastPoll = now

	if in.Err != nil {
		s.FailCount++
		return s, append(acts, logf("network probe failed (%d in a row): %v", s.FailCount, in.Err))
	}
	s.FailCount = 0
	snap := in.Snap
	s.Snap = &snap
	s.Probed = true
	diag := diagLine(snap)
	if r.Mode == contracts.NetworkHotspotAuto && hotspotConnected(snap) {
		acts = append(acts, Action{Kind: ActPreferHotspot, Msg: "hotspot connected"})
	}

	link := snap.Link()

	if snap.DefaultIface != "" && snap.Online {
		if !s.NudgeOnAt.IsZero() {
			result := NudgeOtherNetwork
			if link == contracts.LinkHotspotWiFi {
				result = NudgeLandedHotspot
			}
			acts = append(acts, r.nudgeResult(&s, result, link, now))
		}
		if !s.OfflineSince.IsZero() {
			acts = append(acts, r.backOnline(&s, diag, link, now)...)
		}
		s.Online = true
		// Never nudge while online: note a non-hotspot Wi-Fi once instead.
		if r.nudgeEnabled() && link == contracts.LinkWiFi && !snap.IPhoneUSBPresent && !s.OtherWiFiNoted {
			s.OtherWiFiNoted = true
			acts = append(acts, logf("online via non-hotspot Wi-Fi: not nudging toward the hotspot (only while offline)"))
		}
		return s, acts
	}

	// Offline.
	if s.OfflineSince.IsZero() {
		s.Online = false
		s.OfflineSince = now
		s.First = fmt.Sprintf("%s first: %s", humanize.Clock(now, now), diag)
		s.Latest, s.Notes, s.OutageRecoveries, s.SkipNoted = "", nil, 0, false
		s.OutageNudges, s.FailingNotified = 0, false
		acts = append(acts, logf("offline: %s", diag))
	} else {
		s.Latest = fmt.Sprintf("%s latest: %s", humanize.Clock(now, now), diag)
		acts = append(acts, logf("still offline (%s): %s", humanize.Duration(now.Sub(s.OfflineSince)), diag))
	}
	offFor := now.Sub(s.OfflineSince)

	due := s.LastRecovery.IsZero() || now.Sub(s.LastRecovery) >= r.Cfg.RecoveryMinInterval
	if r.Cfg.Watchdog && offFor >= r.Cfg.RecoveryAfter && due {
		if snap.IfacePresent {
			s.LastRecovery = now
			acts = append(acts, Action{Kind: ActRecover, Msg: fmt.Sprintf("offline %s", humanize.Duration(offFor))})
		} else if !s.SkipNoted {
			s.SkipNoted = true
			acts = append(acts, logf("recovery skipped: hotspot interface not present"))
		}
	}

	if !s.NudgeOnAt.IsZero() && now.Sub(s.NudgeOnAt) >= WiFiNudgeLandWindow {
		acts = append(acts, r.nudgeResult(&s, NudgeStillOffline, link, now))
	}
	nudgeDue := s.LastNudge.IsZero() || now.Sub(s.LastNudge) >= r.Cfg.WiFiNudgeEvery
	if r.nudgeEnabled() && !snap.IPhoneUSBPresent && offFor >= r.Cfg.WiFiNudgeAfter && nudgeDue &&
		!s.NudgeInFlight && s.NudgeOnAt.IsZero() {
		acts = append(acts, r.startNudge(&s, snap, offFor, now)...)
	}
	if a, ok := r.recoveryFailing(&s, diag, now); ok {
		acts = append(acts, a)
	}
	return s, acts
}

// recoveryFailing raises one alert per outage once recoveryFailAttempts
// USB toggles and Wi-Fi nudges have not brought the link back: the last
// attempt started at least WiFiNudgeLandWindow ago and none is in flight.
func (r networkRules) recoveryFailing(s *NetFSM, diag string, now time.Time) (Action, bool) {
	attempts := s.OutageRecoveries + s.OutageNudges
	last := s.LastRecovery
	if s.LastNudge.After(last) {
		last = s.LastNudge
	}
	if s.FailingNotified || attempts < recoveryFailAttempts || s.NudgeInFlight || !s.NudgeOnAt.IsZero() ||
		now.Sub(last) < WiFiNudgeLandWindow {
		return Action{}, false
	}
	s.FailingNotified = true
	return notifyAttention(contracts.PriorityHigh, fmt.Sprintf("Still offline after %d recovery attempts", attempts),
		fmt.Sprintf("Offline since %s (%s). USB service toggles: %d, Wi-Fi nudges: %d. Still trying.\nnow: %s",
			humanize.Clock(s.OfflineSince, now), humanize.Duration(now.Sub(s.OfflineSince)), s.OutageRecoveries, s.OutageNudges, diag),
		"warning", "signal_strength"), true
}

// startNudge records a new Wi-Fi nudge and asks for it (ActWiFiNudge).
func (r networkRules) startNudge(s *NetFSM, snap contracts.NetSnapshot, offFor time.Duration, now time.Time) []Action {
	s.NudgeInFlight = true
	s.LastNudge = now
	s.Nudges++
	s.OutageNudges++
	s.Nudge = nudgeRecord{Attempt: s.Nudges, Reason: "offline " + humanize.Duration(offFor), PowerOnOnly: !snap.WiFiOn,
		Started: now, OfflineFor: offFor}
	return []Action{{Kind: ActWiFiNudge, Msg: s.Nudge.Reason}}
}

// outcome describes a decided nudge: "landed on the hotspot (Wi-Fi) after
// 19s", "online via Wi-Fi instead after 15s", "still offline after 45s" or
// "failed: ...".
func (n nudgeRecord) outcome() string {
	switch n.Result {
	case NudgeLandedHotspot:
		return "landed on the hotspot (Wi-Fi) after " + humanize.Duration(n.After)
	case NudgeOtherNetwork:
		return fmt.Sprintf("online via %s instead after %s", n.Link.Label(), humanize.Duration(n.After))
	case NudgeStillOffline:
		return "still offline after " + humanize.Duration(n.After)
	case NudgeFailed:
		return "failed: " + n.Err
	}
	return "pending"
}

// NudgeDone records that a nudge's Wi-Fi is back on. err is the off
// phase's failure (Wi-Fi was turned back on right away): the nudge then
// counts as failed. Otherwise the next probes classify the result.
func (r networkRules) NudgeDone(s NetFSM, err error, now time.Time) (NetFSM, []Action) {
	s = s.clone()
	s.NudgeInFlight = false
	how := "Wi-Fi off/on"
	if s.Nudge.PowerOnOnly {
		how = "Wi-Fi on"
	}
	if err != nil {
		s.Nudge.Result, s.Nudge.Err, s.Nudge.Link = NudgeFailed, err.Error(), ""
		s.NudgeResults++
		if !s.OfflineSince.IsZero() {
			s.Notes = append(s.Notes, fmt.Sprintf("%s nudge: %s FAILED: %v", humanize.Clock(now, now), how, err))
		}
		return s, []Action{logf("wifi nudge %d failed: %v", s.Nudge.Attempt, err)}
	}
	s.Nudge.OnAt, s.NudgeOnAt = now, now
	if !s.OfflineSince.IsZero() {
		s.Notes = append(s.Notes, fmt.Sprintf("%s nudge: %s (%s)", humanize.Clock(now, now), how, s.Nudge.Reason))
	}
	return s, []Action{logf("wifi nudge %d: %s; waiting up to %s for the hotspot", s.Nudge.Attempt, how, humanize.Duration(WiFiNudgeLandWindow))}
}

func (r networkRules) nudgeResult(s *NetFSM, result string, link contracts.NetLink, now time.Time) Action {
	s.Nudge.Result, s.Nudge.Link, s.Nudge.After = result, link, now.Sub(s.NudgeOnAt)
	s.NudgeOnAt = time.Time{}
	s.NudgeResults++
	what := s.Nudge.outcome()
	if !s.OfflineSince.IsZero() {
		s.Notes = append(s.Notes, fmt.Sprintf("%s nudge: %s", humanize.Clock(now, now), what))
	}
	return logf("wifi nudge %d: %s", s.Nudge.Attempt, what)
}

// hotspotConnected reports whether the hotspot link is up enough to prefer
// it: its interface exists and its gateway answers. A present interface
// alone is not enough; preferring a phone that is plugged in but not
// sharing would move the default route onto a dead link.
func hotspotConnected(n contracts.NetSnapshot) bool {
	return n.IfacePresent && n.GatewayReachable
}

// RecoveryDone records the outcome of an ActRecover toggle that ran, and
// counts it.
func (r networkRules) RecoveryDone(s NetFSM, err error, now time.Time) (NetFSM, []Action) {
	s = s.clone()
	s.Recoveries++
	if !s.OfflineSince.IsZero() {
		s.OutageRecoveries++
	}
	svc := r.Cfg.Service
	if err != nil {
		if !s.OfflineSince.IsZero() {
			s.Notes = append(s.Notes, fmt.Sprintf("%s recovery: toggled %q off/on FAILED: %v", humanize.Clock(now, now), svc, err))
		}
		return s, []Action{logf("recovery: toggling %q failed: %v", svc, err)}
	}
	if !s.OfflineSince.IsZero() {
		s.Notes = append(s.Notes, fmt.Sprintf("%s recovery: toggled %q off/on ok", humanize.Clock(now, now), svc))
	}
	return s, []Action{logf("recovery: toggled %q off/on", svc)}
}

func (r networkRules) backOnline(s *NetFSM, diag string, link contracts.NetLink, now time.Time) []Action {
	dur := now.Sub(s.OfflineSince)
	via := link.Label()
	if link == contracts.LinkOther && s.Snap != nil && s.Snap.DefaultIface != "" {
		via = s.Snap.DefaultIface
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Offline %s-%s (%s).\n", humanize.Clock(s.OfflineSince, now), humanize.Clock(now, now), humanize.Duration(dur))
	for _, line := range append([]string{s.First, s.Latest}, s.Notes...) {
		if line != "" {
			b.WriteString(line + "\n")
		}
	}
	fmt.Fprintf(&b, "Recovery attempts: %d\n", s.OutageRecoveries)
	if r.nudgeEnabled() || s.OutageNudges > 0 {
		fmt.Fprintf(&b, "Wi-Fi nudges: %d\n", s.OutageNudges)
	}
	fmt.Fprintf(&b, "now: %s", diag)
	// A short blip is info; a long outage, or one that raised a "recovery
	// failing" alert, needs attention (it closes that alert).
	class := contracts.ClassInfo
	if dur >= r.OutageMin || s.FailingNotified {
		class = contracts.ClassAttention
	}
	back := notifyClass(class, contracts.PriorityDefault, fmt.Sprintf("Mac back online after %s via %s", humanize.Duration(dur), via), b.String(), "signal_strength")
	back.NextPoll = true
	acts := []Action{logf("back online after %s via %s", humanize.Duration(dur), via), back}
	s.OfflineSince = time.Time{}
	s.First, s.Latest, s.Notes, s.OutageRecoveries, s.SkipNoted = "", "", nil, 0, false
	s.OutageNudges, s.FailingNotified = 0, false
	return acts
}

// diagLine renders a probe as one line: the provider's Diag with lines
// joined, or a summary built from the fields when Diag is empty.
func diagLine(n contracts.NetSnapshot) string {
	if d := strings.TrimSpace(n.Diag); d != "" {
		return strings.Join(strings.Fields(strings.ReplaceAll(d, "\n", "; ")), " ")
	}
	route := n.DefaultIface
	if route == "" {
		route = "none"
	}
	iface := n.Iface
	if iface == "" {
		iface = "hotspot"
	}
	present := "missing"
	if n.IfacePresent {
		present = "yes"
	}
	ip := n.IfaceIPv4
	if ip == "" {
		ip = "-"
	}
	gw := "fail"
	if n.GatewayReachable {
		gw = "ok"
	}
	return fmt.Sprintf("route=%s %s=%s ipv4=%s ping_gw=%s iphone_usb=%s wifi=%s online=%s",
		route, iface, present, ip, gw, yesNo(n.IPhoneUSBPresent), humanize.OnOff(n.WiFiOn), yesNo(n.Online))
}

func (s NetFSM) clone() NetFSM {
	s.Notes = append([]string(nil), s.Notes...)
	if s.Snap != nil {
		c := *s.Snap
		s.Snap = &c
	}
	return s
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}
