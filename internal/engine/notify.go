package engine

import (
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/ilias-t/bckp/internal/config"
	"github.com/ilias-t/bckp/internal/contracts"
	"github.com/ilias-t/bckp/internal/humanize"
)

// Notifications: what the engine sends, and when (start, stop, heartbeat,
// held "back online" notes). Delivery is sendqueue.go's.

// retryOutbox asks the sender to flush the outbox when messages wait and
// the Mac is online (a "back online" sent over a link seconds old often
// fails and is queued), at most once per outboxFlushInterval.
func (e *Engine) retryOutbox(now time.Time) {
	if e.q == nil || e.ob == nil || now.Before(e.nextOutboxFlush) || e.ob.Pending() == 0 {
		return
	}
	e.nextOutboxFlush = now.Add(outboxFlushInterval)
	e.q.requestFlush()
}

// sendHeld sends the notifications held for one poll (Action.NextPoll).
// They go out even if the link dropped again: the outage they report did
// end, and a failed send waits in the outbox.
func (e *Engine) sendHeld(now time.Time) {
	held := e.held
	e.held = nil
	for _, n := range held {
		e.send(now, n)
	}
}

// send is the one place notifications leave the engine (never blocks).
// Attention notifications, and every one with notify.push = "all", are
// queued for the notifier; info ones are only logged (notify.logged) and
// kept in the snapshot's recent list as NotificationLogged.
func (e *Engine) send(now time.Time, n contracts.Notification) {
	if n.Class == "" {
		n.Class = contracts.ClassAttention
	}
	push := n.Class == contracts.ClassAttention || e.cfg.Notify.Push == config.PushAll
	if !push {
		e.ev(slog.LevelInfo, "notify.logged", "title", n.Title, "priority", n.Priority, "class", string(n.Class),
			"pushed", false, "body", n.Body)
		e.recordLogged(now, n)
		return
	}
	e.ev(slog.LevelDebug, "notify.enqueue", "title", n.Title, "priority", n.Priority, "class", string(n.Class), "pushed", true,
		"body", n.Body)
	if e.q == nil {
		return
	}
	e.q.enqueue(now, n)
}

// recordLogged adds a log-only notification to the recent list.
func (e *Engine) recordLogged(now time.Time, n contracts.Notification) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.recs = append(e.recs, notifRecord{rec: contracts.NotificationRecord{Notification: n, At: now, Status: contracts.NotificationLogged}})
	if len(e.recs) > contracts.MaxSnapshotNotifications {
		e.recs = slices.Delete(e.recs, 0, len(e.recs)-contracts.MaxSnapshotNotifications)
	}
}

func (e *Engine) heartbeat(now time.Time) {
	title := "Mac: unknown"
	if r := e.lastThermal; r != nil {
		title = "Mac: " + r.State.String()
		if r.CPUC != nil {
			title += fmt.Sprintf(", CPU %.0f°C", *r.CPUC)
		}
	}
	e.status = e.statusLine(now)
	e.send(now, contracts.Notification{Priority: e.cfg.Notify.HeartbeatPriority, Title: title, Body: e.status, Tags: []string{"thermometer"},
		Class: contracts.ClassInfo})
}

func (e *Engine) notifyStart(now time.Time) {
	e.status = e.statusLine(now)
	title := "backpack on until " + humanize.Clock(e.sess.EndsAt, now)
	body := fmt.Sprintf("Cap %s (%s max).", humanize.Clock(e.sess.CapAt, now), humanize.Duration(e.cfg.Session.MaxDuration))
	if e.sess.Forever {
		title = "backpack on (forever)"
		body = fmt.Sprintf("Runs until stopped; cap %s (%s max).", humanize.Clock(e.sess.CapAt, now), humanize.Duration(e.cfg.Session.MaxDuration))
	}
	body += " " + e.networkSummary()
	e.send(now, contracts.Notification{Priority: contracts.PriorityDefault, Title: title, Body: body + " " + e.status, Tags: []string{"school_satchel"},
		Class: contracts.ClassInfo})
}

// networkSummary describes the session's network options for the start
// notification: "Hotspot auto (waiting for the phone), watchdog on."
func (e *Engine) networkSummary() string {
	hs := string(e.hotspot)
	switch {
	case e.hotspot == config.HotspotAuto && e.mode == contracts.NetworkHotspotAuto:
		hs += " (waiting for the phone)"
	case e.hotspot == config.HotspotAuto && e.mode == contracts.NetworkPreferHotspot:
		hs += " (hotspot preferred)"
	case e.hotspot != config.HotspotOff && e.mode != contracts.NetworkPreferHotspot && e.mode != contracts.NetworkHotspotAuto:
		hs += " (failed; see status)"
	}
	return fmt.Sprintf("Hotspot %s, watchdog %s.", hs, humanize.OnOff(e.cfg.Network.Watchdog))
}

func (e *Engine) notifyStop(now time.Time) {
	ran := humanize.Duration(now.Sub(e.sess.StartedAt))
	var n contracts.Notification
	// Ends the user asked for (stop, stop --sleep, the requested duration)
	// are info; any other end needs attention.
	switch e.endReason {
	case contracts.EndBatteryFloor, contracts.EndLeaseLost:
		return // their own notification already went out
	case contracts.EndThermalSleep:
		n = contracts.Notification{Priority: contracts.PriorityHigh, Title: "Mac sleeping now (thermal)", Tags: []string{"zzz"}, Class: contracts.ClassAttention}
	case contracts.EndUserSleep:
		n = contracts.Notification{Priority: contracts.PriorityDefault, Title: "backpack off - sleeping Mac", Tags: []string{"zzz"}, Class: contracts.ClassInfo}
	case contracts.EndExpired:
		n = contracts.Notification{Priority: contracts.PriorityDefault, Title: "backpack off: time's up", Tags: []string{"hourglass"}, Class: contracts.ClassInfo}
	case contracts.EndMaxDuration:
		n = contracts.Notification{Priority: contracts.PriorityHigh, Title: fmt.Sprintf("backpack off: %s cap reached", humanize.Duration(e.cfg.Session.MaxDuration)), Tags: []string{"hourglass"}, Class: contracts.ClassAttention}
	case contracts.EndError:
		n = contracts.Notification{Priority: contracts.PriorityUrgent, Title: "backpack stopped: engine error", Tags: []string{"warning"}, Class: contracts.ClassAttention}
	case contracts.EndStopped:
		n = contracts.Notification{Priority: contracts.PriorityDefault, Title: "backpack off", Tags: []string{"school_satchel"}, Class: contracts.ClassInfo}
	default: // EndSignal: stopped by a signal, not by `backpack stop`
		n = contracts.Notification{Priority: contracts.PriorityDefault, Title: "backpack off (" + string(e.endReason) + ")", Tags: []string{"school_satchel"}, Class: contracts.ClassAttention}
	}
	// Ending on battery needs attention whatever the reason: the Mac is
	// unplugged and sleeps (or drains) in the bag from now on.
	if e.pw.Known && !e.pw.OnAC && n.Class == contracts.ClassInfo {
		n.Class, n.Priority = contracts.ClassAttention, max(n.Priority, contracts.PriorityHigh)
		n.Title += fmt.Sprintf(" (on battery, %d%%)", e.pw.Percent)
	}
	n.Body = fmt.Sprintf("Ran %s (%s). The Mac sleeps normally when the lid is closed. %s", ran, e.endMsg, e.status)
	if e.sleepAfter {
		n.Body = fmt.Sprintf("Ran %s (%s). %s", ran, e.endMsg, e.status)
	}
	e.send(now, n)
}

// flushNotifications gives the queued notifications FlushTimeout to go
// out. Sends cut off at the deadline fail at once, so the outbox (when the
// notifier has one) keeps them for later instead of losing them.
func (e *Engine) flushNotifications() {
	if cut := e.q.flush(FlushTimeout); cut > 0 {
		e.ev(slog.LevelWarn, "notify.flush_timeout", "timeout", dur(FlushTimeout), "cut_off", cut)
	}
}
