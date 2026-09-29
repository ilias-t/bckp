package engine

import (
	"context"
	"log/slog"
	"strconv"
	"time"

	"github.com/ilias-t/bckp/internal/contracts"
	"github.com/ilias-t/bckp/internal/version"
)

// Structured event log (internal/eventlog; taxonomy in
// internal/eventlog/events.md). Every record carries an "event" field
// naming its kind; Deps.Logger decides where it goes and redacts the ntfy
// topic.

const (
	// sampleInterval is how often a condensed "sample" reading is logged.
	sampleInterval = time.Minute
	// providerErrInterval rate-limits provider.error per provider.
	providerErrInterval = time.Minute
	// sleepGapFactor: a poll gap longer than this many poll intervals is
	// logged as suspected_sleep.
	sleepGapFactor = 3
)

// ev logs one event: msg and the event field are both name.
func (e *Engine) ev(level slog.Level, name string, args ...any) {
	ctx := context.Background()
	if !e.log.Enabled(ctx, level) {
		return
	}
	e.log.Log(ctx, level, name, append([]any{"event", name}, args...)...)
}

func dur(d time.Duration) string { return d.Round(time.Second).String() }

// logStart records session.start: version, options and a config summary.
// The ntfy topic is never logged, only whether one is set.
func (e *Engine) logStart(opts Options) {
	c := e.cfg
	cpuSleep := "off"
	if c.Thermal.CPUSleepC != nil {
		cpuSleep = strconv.FormatFloat(*c.Thermal.CPUSleepC, 'f', -1, 64) + "°C"
	}
	ends := "forever"
	if !e.sess.Forever {
		ends = e.sess.EndsAt.Format(time.RFC3339)
	}
	args := []any{
		"version", version.String(),
		"pid", e.pid,
		slog.Group("options",
			"duration", dur(opts.Duration), "forever", e.sess.Forever, "hotspot", string(e.hotspot),
			"watchdog", c.Network.Watchdog, "network", string(e.mode)),
		"ends", ends, "cap_at", e.sess.CapAt.Format(time.RFC3339),
		slog.Group("config",
			"poll", dur(c.Session.Poll), "max_duration", dur(c.Session.MaxDuration),
			"battery_floor", c.Power.BatteryFloor, "cpu_sleep", cpuSleep,
			"serious_sleep_after", dur(c.Thermal.SeriousSleepAfter),
			"watchdog", c.Network.Watchdog, "prefer_hotspot", string(c.Network.PreferHotspot),
			"service", c.Network.Service,
			"wifi_nudge_after", dur(c.Network.WiFiNudgeAfter), "wifi_nudge_every", dur(c.Network.WiFiNudgeEvery),
			"ntfy", c.Notify.NtfyTopic != "", "macos_notify", c.Notify.MacOS,
			"heartbeat", dur(c.Notify.Heartbeat), "push", c.Notify.Push, "outage_min", dur(c.Notify.OutageMin)),
	}
	if len(e.choices) > 0 {
		args = append(args, "choices", choiceAttrs(e.choices))
	}
	e.ev(slog.LevelInfo, "session.start", args...)
}

// choiceAttrs renders the CLI's session options for session.start as
// {"duration":"forever (default)", "hotspot":"auto (prompt)", ...}.
func choiceAttrs(cs []contracts.SessionOption) map[string]string {
	if len(cs) == 0 {
		return nil
	}
	m := make(map[string]string, len(cs))
	for _, c := range cs {
		m[c.Name] = c.Value + " (" + c.Source + ")"
	}
	return m
}

// thermalAttrs are the readings behind a thermal event.
func thermalAttrs(r *contracts.ThermalReading) []any {
	if r == nil {
		return []any{"thermal", "unknown"}
	}
	out := []any{"thermal", r.State.String()}
	if r.CPUC != nil {
		out = append(out, "cpu_c", round1(*r.CPUC))
	}
	if r.GPUC != nil {
		out = append(out, "gpu_c", round1(*r.GPUC))
	}
	return out
}

func round1(f float64) float64 { return float64(int64(f*10+0.5)) / 10 }

func powerSrc(onAC bool) string {
	if onAC {
		return "AC"
	}
	return "battery"
}

func lidState(closed bool) string {
	if closed {
		return "closed"
	}
	return "open"
}

func onlineState(online bool) string {
	if online {
		return "online"
	}
	return "offline"
}

// sample logs the condensed once-a-minute reading.
func (e *Engine) sample(now time.Time) {
	if !e.nextSample.IsZero() && now.Before(e.nextSample) {
		return
	}
	e.nextSample = now.Add(sampleInterval)
	args := append([]any{"fsm", string(e.th.State)}, thermalAttrs(e.lastThermal)...)
	if l, err := e.d.LoadAvg(); err == nil {
		args = append(args, "load", float64(int64(l*100+0.5))/100)
	}
	if p := e.lastPower; p != nil {
		args = append(args, "battery", p.Percent, "ac", p.OnAC, "charging", p.Charging,
			"lid", lidState(p.LidClosed), "sleep_disabled", p.SleepDisabled)
	}
	if e.mode != contracts.NetworkOff {
		if s := e.nw.Snap; s != nil {
			iface := s.DefaultIface
			if iface == "" {
				iface = "none"
			}
			args = append(args, "net", iface, "link", s.Link().Label(), "online", s.Online)
			if e.nw.Nudges > 0 {
				args = append(args, "wifi_nudges", e.nw.Nudges)
			}
		} else {
			args = append(args, "net", "unknown")
		}
	}
	end, _ := e.effectiveEnd()
	args = append(args, "left", dur(end.Sub(now)))
	e.ev(slog.LevelDebug, "sample", args...)
}

// suspectSleep logs suspected_sleep when the gap since the previous poll
// is far longer than the poll interval (the Mac, or the engine, was not
// running).
func (e *Engine) suspectSleep(now time.Time) {
	prev := e.lastPollAt
	e.lastPollAt = now
	poll := e.cfg.Session.Poll
	if prev.IsZero() || poll <= 0 {
		return
	}
	if gap := now.Sub(prev); gap > sleepGapFactor*poll {
		e.ev(slog.LevelWarn, "suspected_sleep", "gap", dur(gap), "expected", dur(poll), "last_poll", prev,
			"hint", "cross-check the macos pm.* events")
	}
}

// providerErr tracks one provider's read result: provider.error at most
// once per providerErrInterval (with the count suppressed in between) and
// provider.recovered, with the failure count, once it reads again.
type providerErr struct {
	since      time.Time
	lastLog    time.Time
	failures   int
	suppressed int
}

func (e *Engine) providerResult(now time.Time, kind string, err error) {
	p := e.provErrs[kind]
	if err == nil {
		if p != nil {
			e.ev(slog.LevelInfo, "provider.recovered", "kind", kind, "failures", p.failures, "for", dur(now.Sub(p.since)))
			delete(e.provErrs, kind)
		}
		return
	}
	if p == nil {
		p = &providerErr{since: now}
		e.provErrs[kind] = p
	}
	p.failures++
	if !p.lastLog.IsZero() && now.Sub(p.lastLog) < providerErrInterval {
		p.suppressed++
		return
	}
	e.ev(slog.LevelWarn, "provider.error", "kind", kind, "err", err,
		"failures", p.failures, "suppressed", p.suppressed)
	p.lastLog, p.suppressed = now, 0
}

// noteNetwork logs outage start/end, diagnostics changes and online/offline
// transitions for a network state change (old -> nw).
func (e *Engine) noteNetwork(now time.Time, old, nw NetFSM) {
	if !nw.Probed || nw.Snap == nil {
		return
	}
	diag := diagLine(*nw.Snap)
	link := nw.Snap.Link()
	if (old.Probed && nw.Online != old.Online) || (!old.Probed && !nw.Online) || linkChanged(old, nw) {
		from, fromLink := "unknown", "unknown"
		if old.Probed {
			from = onlineState(old.Online)
			if old.Snap != nil {
				fromLink = old.Snap.Link().Label()
			}
		}
		e.ev(slog.LevelInfo, "net.transition", "from", from, "to", onlineState(nw.Online),
			"from_link", fromLink, "link", link.Label(), "iface", nw.Snap.DefaultIface, "diag", diag)
	}
	if nw.NudgeResults != old.NudgeResults {
		e.logNudge(nw)
	}
	switch {
	case old.OfflineSince.IsZero() && !nw.OfflineSince.IsZero():
		e.lastDiag = diag
		e.ev(slog.LevelWarn, "net.outage_start", append([]any{"diag", diag, "hotspot_present", nw.Snap.IfacePresent},
			checkAttrs(nw.Snap.Checks)...)...)
	case !old.OfflineSince.IsZero() && nw.OfflineSince.IsZero():
		e.lastDiag = ""
		e.ev(slog.LevelInfo, "net.outage_end", "duration", dur(now.Sub(old.OfflineSince)),
			"recoveries", old.OutageRecoveries, "first", old.First, "diag", diag)
	case !nw.OfflineSince.IsZero() && diag != e.lastDiag:
		e.lastDiag = diag
		e.ev(slog.LevelInfo, "net.diag", append([]any{"offline_for", dur(now.Sub(nw.OfflineSince)), "diag", diag},
			checkAttrs(nw.Snap.Checks)...)...)
	}
}

// checkAttrs tags an outage event with the DNS/IP checks (dns_sys,
// dns_direct, ip_ok, and a verdict when they point somewhere); none when
// the probe did not run them.
func checkAttrs(c *contracts.NetChecks) []any {
	if c == nil {
		return nil
	}
	args := []any{"dns_sys", c.DNSSys, "dns_direct", c.DNSDirect, "ip_ok", c.IPOK}
	if v := c.Verdict(); v != "" {
		args = append(args, "verdict", v)
	}
	return args
}

// linkChanged reports a change of link while online on both probes (for
// example Wi-Fi to the hotspot over Wi-Fi). Online/offline changes are
// reported on their own.
func linkChanged(old, nw NetFSM) bool {
	return old.Probed && nw.Probed && old.Online && nw.Online && old.Snap != nil && nw.Snap != nil &&
		old.Snap.Link() != nw.Snap.Link()
}

// logNudge records a decided Wi-Fi nudge (net.wifi_nudge).
func (e *Engine) logNudge(nw NetFSM) {
	n := nw.Nudge
	level := slog.LevelInfo
	if n.Result == NudgeStillOffline || n.Result == NudgeFailed {
		level = slog.LevelWarn
	}
	args := []any{"reason", n.Reason, "result", n.Result, "outcome", n.outcome(), "attempt", n.Attempt,
		"power_on_only", n.PowerOnOnly, "offline_for", dur(n.OfflineFor)}
	switch n.Result {
	case NudgeLandedHotspot:
		args = append(args, "landed_after", dur(n.After), "link", n.Link.Label())
	case NudgeOtherNetwork:
		args = append(args, "online_after", dur(n.After), "link", n.Link.Label())
	case NudgeStillOffline:
		args = append(args, "waited", dur(n.After))
	case NudgeFailed:
		args = append(args, "err", n.Err)
	}
	if !n.OnAt.IsZero() {
		args = append(args, "toggle", dur(n.OnAt.Sub(n.Started)))
	}
	e.ev(level, "net.wifi_nudge", args...)
}
