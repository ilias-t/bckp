package engine

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/ilias-t/bckp/internal/contracts"
	"github.com/ilias-t/bckp/internal/humanize"
)

// Snapshots: the engine goroutine publishes one after every change;
// Snapshot and Subscribe read the latest under e.mu.

// Snapshot returns the current snapshot.
func (e *Engine) Snapshot() contracts.Snapshot {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.snap
}

// Subscribe returns a channel that yields the current snapshot, then one
// per engine wake-up (every poll at least). Slow readers see only the
// latest snapshot (older ones are coalesced away). The channel closes
// after the final snapshot (Session.Ended set) or when ctx ends.
func (e *Engine) Subscribe(ctx context.Context) <-chan contracts.Snapshot {
	ch := make(chan contracts.Snapshot, 1)
	e.mu.Lock()
	ch <- e.snap
	if e.ended {
		close(ch)
		e.mu.Unlock()
		return ch
	}
	sub := &subscriber{ch: ch, gone: make(chan struct{})}
	e.subs[sub] = struct{}{}
	e.mu.Unlock()
	go func() {
		select {
		case <-ctx.Done():
			e.mu.Lock()
			if _, ok := e.subs[sub]; ok {
				delete(e.subs, sub)
				close(sub.ch)
			}
			e.mu.Unlock()
		case <-sub.gone:
		}
	}()
	return ch
}

// offer delivers s to a 1-buffered channel without blocking, replacing an
// unread older snapshot. Callers hold e.mu, the only sender.
func offer(ch chan contracts.Snapshot, s contracts.Snapshot) {
	select {
	case ch <- s:
		return
	default:
	}
	select {
	case <-ch:
	default:
	}
	select {
	case ch <- s:
	default:
	}
}

// publish builds the current snapshot and fans it out.
func (e *Engine) publish(now time.Time) contracts.Snapshot {
	snap := e.buildSnapshot(now)
	e.mu.Lock()
	defer e.mu.Unlock()
	e.attachLocked(&snap)
	e.snap = snap
	for sub := range e.subs {
		offer(sub.ch, snap)
	}
	return snap
}

// republishLocked refreshes notification records in the current snapshot
// (called by the sender goroutine).
func (e *Engine) republishLocked() {
	snap := e.snap
	e.attachLocked(&snap)
	e.snap = snap
	for sub := range e.subs {
		offer(sub.ch, snap)
	}
}

// attachLocked sets the fields owned by the sender goroutine.
func (e *Engine) attachLocked(s *contracts.Snapshot) {
	s.Notifications = nil
	for _, r := range e.recs {
		s.Notifications = append(s.Notifications, r.rec)
	}
	errs := map[string]string{}
	for k, v := range s.Errors {
		errs[k] = v
	}
	delete(errs, "notify")
	if e.notifyErr != "" {
		errs["notify"] = e.notifyErr
	}
	s.Errors = nil
	if len(errs) > 0 {
		s.Errors = errs
	}
	if e.ob != nil {
		s.OutboxPending = e.ob.Pending()
	}
}

func (e *Engine) buildSnapshot(now time.Time) contracts.Snapshot {
	s := contracts.Snapshot{At: now, PID: e.pid}
	s.Session = e.sess
	s.Options = append([]contracts.SessionOption(nil), e.choices...)
	if !e.sess.StartedAt.IsZero() {
		end, _ := e.effectiveEnd()
		s.Session.RemainingS = max(0, int64(end.Sub(now)/time.Second))
		if e.sess.Ended {
			s.Session.RemainingS = 0
		}
	}
	s.Thermal = contracts.ThermalStatus{FSM: e.th.State, HotSince: e.th.HotSince}
	if e.lastThermal != nil {
		r := *e.lastThermal
		s.Thermal.Reading = &r
	}
	if e.lastPower != nil {
		p := *e.lastPower
		s.Power = &p
	}
	s.Network = contracts.NetworkStatus{Mode: e.mode}
	if e.mode != contracts.NetworkOff {
		s.Network.Online = e.nw.Online
		s.Network.OfflineSince = e.nw.OfflineSince
		s.Network.LastRecovery = e.nw.LastRecovery
		s.Network.Recoveries = e.nw.Recoveries
		s.Network.WiFiNudges = e.nw.Nudges
		s.Network.LastWiFiNudge = e.nw.LastNudge
		if e.nw.Nudges > 0 {
			s.Network.LastWiFiNudgeResult = e.nw.Nudge.Result
		}
		if e.nw.Snap != nil {
			n := *e.nw.Snap
			s.Network.Latest = &n
			s.Network.Link = n.Link()
		}
	}
	if e.lease != nil && !e.sess.Ended {
		s.Lease = contracts.LeaseStatus{Held: true, ID: e.lease.ID(), Expires: e.lease.Expires()}
	}
	if p := e.th.Pending; p != nil {
		s.SleepPending = &contracts.SleepPending{
			Trigger: p.Trigger, Reason: p.Reason, At: p.At, Abortable: p.Abortable,
			RemainingS: max(0, int64(p.At.Sub(now)/time.Second)),
		}
	}
	if len(e.errs) > 0 {
		s.Errors = make(map[string]string, len(e.errs))
		for k, v := range e.errs {
			s.Errors[k] = v
		}
	}
	return s
}

// statusLine renders the one-line machine status used in notification
// bodies: thermal and °C, load, power, lid, network, time left.
func (e *Engine) statusLine(now time.Time) string {
	var parts []string
	if r := e.lastThermal; r != nil {
		t := "thermal " + r.State.String()
		if r.CPUC != nil {
			t += fmt.Sprintf(", CPU %.0f°C", *r.CPUC)
		}
		if r.GPUC != nil {
			t += fmt.Sprintf(" GPU %.0f°C", *r.GPUC)
		}
		parts = append(parts, t)
	} else {
		parts = append(parts, "thermal ?")
	}
	if l, err := e.d.LoadAvg(); err == nil {
		parts = append(parts, fmt.Sprintf("load %.2f", l))
	}
	if p := e.lastPower; p != nil {
		lid := "lid open"
		if p.LidClosed {
			lid = "lid closed"
		}
		parts = append(parts, powerLabel(*p), lid)
	} else {
		parts = append(parts, "power ?")
	}
	if e.mode != contracts.NetworkOff {
		net := "net ?"
		if s := e.nw.Snap; s != nil {
			net = "net " + s.DefaultIface
			if s.DefaultIface == "" {
				net = "net none"
			} else {
				net += " " + s.Link().Label()
			}
			if !s.Online {
				net += " (offline)"
			}
		}
		parts = append(parts, net)
	}
	if !e.sess.StartedAt.IsZero() {
		end, why := e.effectiveEnd()
		left := humanize.Duration(end.Sub(now)) + " left"
		if why == contracts.EndMaxDuration && e.sess.Forever {
			left = "forever, cap in " + humanize.Duration(end.Sub(now))
		}
		parts = append(parts, left)
	}
	return strings.Join(parts, " | ")
}
