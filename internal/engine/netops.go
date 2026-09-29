package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/ilias-t/bckp/internal/config"
	"github.com/ilias-t/bckp/internal/contracts"
)

// setupNetwork prepares the session's network mode: it restores a stale
// journal, writes a fresh one (original service order and Wi-Fi power), and
// in prefer_hotspot mode moves the hotspot service ahead of Wi-Fi (auto
// mode waits for engageHotspot). Any failure drops the hotspot preference
// (dropHotspot) and is reported, never fatal.
func (e *Engine) setupNetwork(ctx context.Context, now time.Time) {
	if e.mode == contracts.NetworkOff {
		return
	}
	n, svc := e.d.Network, e.cfg.Network.Service
	degrade := func(why string, err error) {
		e.errs["network"] = fmt.Sprintf("%s: %v", why, err)
		e.ev(slog.LevelError, "net.setup_error", "step", why, "err", err, "mode", string(e.mode))
		if e.mode == contracts.NetworkPreferHotspot || e.mode == contracts.NetworkHotspotAuto {
			e.dropHotspot(now, why, err)
		}
	}

	cctx, cancel := context.WithTimeout(ctx, providerTimeout)
	defer cancel()
	restored, err := RestoreStaleJournal(cctx, n, e.d.Paths)
	if err != nil {
		// Keep the stale journal: overwriting it would lose the original
		// state it records.
		degrade("restore stale journal", err)
		return
	}
	if restored {
		e.ev(slog.LevelWarn, "net.restore", "stale", true, "result", "ok", "path", e.d.Paths.RestoreJournal)
	}

	j := Journal{Version: JournalVersion, CreatedAt: now, PID: e.pid, Service: svc}
	order, oerr := n.ServiceOrder(cctx)
	if oerr == nil {
		j.ServiceOrder = order
	}
	if on, err := n.WiFiPower(cctx); err == nil {
		j.WiFiPower = &on
	} else {
		e.ev(slog.LevelWarn, "net.wifi_unknown", "err", err, "note", "wifi will not be touched")
	}
	if err := writeJournal(e.d.Paths.RestoreJournal, j); err != nil {
		degrade("write restore journal", err)
		return
	}
	e.journal = &j
	// The Wi-Fi nudge needs the original Wi-Fi power journaled. It does
	// not depend on the service order, so a failed reorder below keeps it.
	e.netRules.WiFiNudge = e.hotspot == config.HotspotOn && j.WiFiPower != nil &&
		e.cfg.Network.WiFiNudgeAfter > 0

	if e.mode != contracts.NetworkPreferHotspot {
		return
	}
	if oerr != nil {
		degrade("read service order", oerr)
		return
	}
	want, changed, err := hotspotOrder(order, svc)
	if err != nil {
		degrade("hotspot order", err)
		return
	}
	if !changed {
		e.ev(slog.LevelInfo, "net.order_change", "changed", false, "order", order)
		return
	}
	if !e.markJournal(func(j *Journal) { j.Reordered = true }) {
		degrade("write restore journal", fmt.Errorf("%s", e.errs["network"]))
		return
	}
	if err := n.SetServiceOrder(cctx, want); err != nil {
		degrade("set service order", err)
		return
	}
	e.ev(slog.LevelInfo, "net.order_change", "changed", true, "from", strings.Join(order, ", "), "to", strings.Join(want, ", "))
}

// dropHotspot gives up the hotspot preference for the rest of the session
// after a failure: the mode falls back to the watchdog (or off), and the
// user is told.
func (e *Engine) dropHotspot(now time.Time, why string, err error) {
	e.mode = e.fallbackMode()
	e.netRules.Mode = e.mode
	rest := "Running with the network watchdog only."
	switch {
	case e.mode == contracts.NetworkOff:
		rest = "The network is no longer watched."
		e.netRules.WiFiNudge = false
	case e.netRules.WiFiNudge:
		rest = "Running with the network watchdog; the Wi-Fi hotspot nudge stays on."
	}
	e.send(now, contracts.Notification{Priority: contracts.PriorityHigh, Title: "Hotspot preference off", Class: contracts.ClassAttention,
		Body: fmt.Sprintf("%s: %v. %s", why, err, rest), Tags: []string{"warning"}})
}

// engageHotspot is auto mode's switch-over, once the hotspot connected:
// move it ahead of Wi-Fi and continue as NetworkPreferHotspot. The journal
// is rewritten first with the order as it is now (the user may have
// changed it since the session started), so the exit restores exactly the
// order this change replaced. A failure drops the preference for the
// session rather than retrying every poll.
func (e *Engine) engageHotspot(ctx context.Context, now time.Time, why string) {
	if e.mode != contracts.NetworkHotspotAuto {
		return
	}
	n, svc := e.d.Network, e.cfg.Network.Service
	fail := func(step string, err error) {
		e.errs["network"] = fmt.Sprintf("%s: %v", step, err)
		e.ev(slog.LevelError, "net.setup_error", "step", step, "err", err, "mode", string(e.mode))
		e.dropHotspot(now, step, err)
	}
	cctx, cancel := context.WithTimeout(ctx, providerTimeout)
	defer cancel()
	order, err := n.ServiceOrder(cctx)
	if err != nil {
		fail("read service order", err)
		return
	}
	want, changed, err := hotspotOrder(order, svc)
	if err != nil {
		fail("hotspot order", err)
		return
	}
	if !changed {
		e.mode, e.netRules.Mode = contracts.NetworkPreferHotspot, contracts.NetworkPreferHotspot
		e.ev(slog.LevelInfo, "net.order_change", "changed", false, "order", order, "trigger", "auto", "why", why)
		return
	}
	if !e.markJournal(func(j *Journal) { j.ServiceOrder, j.Reordered = slices.Clone(order), true }) {
		fail("write restore journal", errors.New(orNone(e.errs["network"])))
		return
	}
	if err := n.SetServiceOrder(cctx, want); err != nil {
		fail("set service order", err)
		return
	}
	e.mode, e.netRules.Mode = contracts.NetworkPreferHotspot, contracts.NetworkPreferHotspot
	e.ev(slog.LevelInfo, "net.order_change", "changed", true, "from", strings.Join(order, ", "), "to", strings.Join(want, ", "),
		"trigger", "auto", "why", why)
}

func orNone(s string) string {
	if s == "" {
		return "no restore journal"
	}
	return s
}

// markJournal applies change to the journal and rewrites it. It reports
// false (and records the error) when the journal cannot be written; the
// caller must then not make the change.
func (e *Engine) markJournal(change func(*Journal)) bool {
	if e.journal == nil {
		return false
	}
	j := *e.journal
	j.ServiceOrder = slices.Clone(j.ServiceOrder)
	change(&j)
	if err := writeJournal(e.d.Paths.RestoreJournal, j); err != nil {
		e.errs["network"] = err.Error()
		e.ev(slog.LevelError, "net.journal_error", "err", err, "path", e.d.Paths.RestoreJournal)
		return false
	}
	e.journal = &j
	return true
}

// startRecovery is phase 1 of a recovery: journal it, disable the hotspot
// service and schedule re-enabling it RecoveryToggleGap later. It refuses
// when the journal cannot record the change: a crash would then leave the
// service disabled.
func (e *Engine) startRecovery(ctx context.Context, now time.Time, why string) {
	svc := e.cfg.Network.Service
	switch {
	case !e.toggle.reenableAt.IsZero():
		return
	case !e.markJournal(func(j *Journal) { j.ServiceDisabled = true }):
		e.ev(slog.LevelWarn, "net.recovery_attempt", "service", svc, "why", why, "result", "refused",
			"err", orNone(e.errs["network"]))
		return
	}
	cctx, cancel := context.WithTimeout(ctx, providerTimeout)
	defer cancel()
	e.recoverWhy = why
	e.ev(slog.LevelDebug, "net.recovery_start", "service", svc, "why", why)
	if err := e.d.Network.SetServiceEnabled(cctx, svc, false); err != nil {
		// Never leave it disabled, even if disabling half-failed.
		enErr := e.d.Network.SetServiceEnabled(cctx, svc, true)
		if enErr == nil {
			e.markJournal(func(j *Journal) { j.ServiceDisabled = false })
		}
		nw, acts := e.netRules.RecoveryDone(e.nw, err, now)
		e.ev(slog.LevelWarn, "net.recovery_attempt", "service", svc, "why", why, "result", "failed",
			"step", "disable", "err", err, "reenabled", enErr == nil, "attempt", nw.OutageRecoveries)
		e.setNet(ctx, now, nw, acts)
		return
	}
	e.toggle.reenableAt = now.Add(RecoveryToggleGap)
	e.toggle.reenableFailed = false
}

// finishRecovery is phase 2: re-enable the service. A failure is retried
// every poll until it succeeds or the session ends (restoreNetwork
// re-enables it in any case). The state machine hears about the toggle
// once: when the service is back on, or when turning it on first fails.
func (e *Engine) finishRecovery(ctx context.Context, now time.Time) {
	svc := e.cfg.Network.Service
	cctx, cancel := context.WithTimeout(ctx, providerTimeout)
	defer cancel()
	err := e.d.Network.SetServiceEnabled(cctx, svc, true)
	if err != nil {
		e.toggle.reenableAt = now.Add(e.cfg.Session.Poll)
		e.errs["network"] = fmt.Sprintf("re-enable %q: %v", svc, err)
		if e.toggle.reenableFailed {
			e.ev(slog.LevelDebug, "net.recovery_retry", "service", svc, "err", err)
			return
		}
		e.toggle.reenableFailed = true
		nw, acts := e.netRules.RecoveryDone(e.nw, err, now)
		e.ev(slog.LevelError, "net.recovery_attempt", "service", svc, "why", e.recoverWhy, "result", "failed",
			"step", "re-enable", "err", err, "note", "retrying every poll", "attempt", nw.OutageRecoveries)
		e.setNet(ctx, now, nw, acts)
		return
	}
	e.toggle.reenableAt = time.Time{}
	e.markJournal(func(j *Journal) { j.ServiceDisabled = false })
	if e.toggle.reenableFailed {
		delete(e.errs, "network")
		e.ev(slog.LevelInfo, "net.recovery_retry", "service", svc, "result", "ok")
		return
	}
	nw, acts := e.netRules.RecoveryDone(e.nw, nil, now)
	e.ev(slog.LevelInfo, "net.recovery_attempt", "service", svc, "why", e.recoverWhy, "result", "ok",
		"attempt", nw.OutageRecoveries)
	e.setNet(ctx, now, nw, acts)
}

// startWiFiNudge is phase 1 of a Wi-Fi nudge (ActWiFiNudge): journal it,
// turn Wi-Fi off and schedule turning it on WiFiNudgeGap later. When Wi-Fi
// is off already, phase 2 runs right away. The nudge never runs without
// the original Wi-Fi power journaled.
func (e *Engine) startWiFiNudge(ctx context.Context, now time.Time, why string) {
	fail := func(err error) {
		nw, acts := e.netRules.NudgeDone(e.nw, err, now)
		e.setNet(ctx, now, nw, acts)
	}
	switch {
	case !e.toggle.nudgeOnAt.IsZero():
		fail(errors.New("the previous nudge is still turning Wi-Fi on"))
		return
	case e.journal == nil || e.journal.WiFiPower == nil:
		fail(errors.New("original Wi-Fi power not journaled"))
		return
	case !e.markJournal(func(j *Journal) { j.WiFiChanged, j.WiFiNudge = true, true }):
		fail(fmt.Errorf("restore journal not writable: %s", orNone(e.errs["network"])))
		return
	}
	n := e.nw.Nudge
	e.toggle.nudgeOffErr, e.toggle.nudgeOnFailed = nil, false
	e.ev(slog.LevelDebug, "net.wifi_nudge_start", "why", why, "attempt", n.Attempt, "power_on_only", n.PowerOnOnly)
	if n.PowerOnOnly {
		e.finishWiFiNudge(ctx, now)
		return
	}
	cctx, cancel := context.WithTimeout(ctx, providerTimeout)
	err := e.d.Network.SetWiFiPower(cctx, false)
	cancel()
	if err != nil {
		// Never leave it off, even if turning off half-failed.
		e.toggle.nudgeOffErr = fmt.Errorf("turn Wi-Fi off: %w", err)
		e.finishWiFiNudge(ctx, now)
		return
	}
	e.toggle.nudgeOnAt = now.Add(WiFiNudgeGap)
}

// finishWiFiNudge is phase 2: turn Wi-Fi on. A failure is retried every
// poll until it succeeds or the session ends (restoreNetwork turns it on
// in any case). The first failure pushes an alert: with Wi-Fi stuck off,
// the Mac may have no way online. The state machine hears about the nudge
// once Wi-Fi is on.
func (e *Engine) finishWiFiNudge(ctx context.Context, now time.Time) {
	cctx, cancel := context.WithTimeout(ctx, providerTimeout)
	err := e.d.Network.SetWiFiPower(cctx, true)
	cancel()
	if err != nil {
		e.toggle.nudgeOnAt = now.Add(e.cfg.Session.Poll)
		e.errs["network"] = fmt.Sprintf("turn Wi-Fi on after a nudge: %v", err)
		if !e.toggle.nudgeOnFailed {
			e.toggle.nudgeOnFailed = true
			e.ev(slog.LevelError, "net.wifi_change", "on", true, "why", "wifi nudge", "result", "failed", "err", err,
				"note", "retrying every poll")
			e.send(now, notifyAttention(contracts.PriorityHigh, "Wi-Fi stuck off after a nudge",
				fmt.Sprintf("backpack turned Wi-Fi off to look for the iPhone hotspot and could not turn it back on (%v). "+
					"Retrying every poll; turn Wi-Fi on by hand if it stays off.", err),
				"warning", "signal_strength").Notification)
		}
		return
	}
	e.toggle.nudgeOnAt = time.Time{}
	if strings.HasPrefix(e.errs["network"], "turn Wi-Fi on after a nudge") {
		delete(e.errs, "network")
	}
	e.markJournal(func(j *Journal) { j.WiFiNudge = false })
	e.ev(slog.LevelDebug, "net.wifi_change", "on", true, "why", "wifi nudge", "result", "ok")
	offErr := e.toggle.nudgeOffErr
	e.toggle.nudgeOffErr = nil
	nw, acts := e.netRules.NudgeDone(e.nw, offErr, now)
	e.setNet(ctx, now, nw, acts)
}

// restoreNetwork undoes the session's network changes and deletes the
// journal. A toggle in flight is undone first, whatever the journal says.
// On failure the journal is kept for RestoreStaleJournal.
func (e *Engine) restoreNetwork() {
	if e.mode == contracts.NetworkOff && e.journal == nil {
		return
	}
	now := e.clock.Now()
	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()
	var errs []error
	if !e.toggle.reenableAt.IsZero() {
		if err := e.d.Network.SetServiceEnabled(ctx, e.cfg.Network.Service, true); err != nil {
			errs = append(errs, fmt.Errorf("re-enable %q: %w", e.cfg.Network.Service, err))
		}
	}
	if !e.toggle.nudgeOnAt.IsZero() {
		if err := e.d.Network.SetWiFiPower(ctx, true); err != nil {
			errs = append(errs, fmt.Errorf("turn Wi-Fi on: %w", err))
		}
	}
	e.toggle = toggles{}
	if e.journal != nil {
		if err := applyJournal(ctx, e.d.Network, *e.journal); err != nil {
			errs = append(errs, err)
		}
	}
	if err := errors.Join(errs...); err != nil {
		e.errs["network"] = "restore: " + err.Error()
		args, body := []any{"result", "failed", "err", err}, err.Error()+"."
		if e.journal != nil {
			args = append(args, "journal_kept", e.d.Paths.RestoreJournal)
			body += " The journal is kept; the next `bckp` start or `bckp doctor --fix` retries it."
		}
		e.ev(slog.LevelError, "net.restore", args...)
		e.send(now, contracts.Notification{Priority: contracts.PriorityHigh, Title: "Network restore failed", Class: contracts.ClassAttention,
			Body: body, Tags: []string{"warning"}})
		return
	}
	if e.journal == nil {
		return
	}
	if err := os.Remove(e.d.Paths.RestoreJournal); err != nil && !os.IsNotExist(err) {
		e.errs["network"] = err.Error()
		return
	}
	if j := e.journal; j.Reordered || j.WiFiChanged || j.ServiceDisabled || j.WiFiNudge {
		e.ev(slog.LevelInfo, "net.restore", "result", "ok", "order", j.Reordered,
			"wifi", j.WiFiChanged, "service", j.ServiceDisabled, "mid_nudge", j.WiFiNudge)
	}
	e.journal = nil
}
