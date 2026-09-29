// Package engine is backpack's user-space session engine: it holds the
// daemon's sleep-disable lease, polls the thermal, power and network
// providers, runs the three state machines (thermal.go, power.go,
// network.go), sends notifications, and serves Snapshots to the CLI/TUI
// (via internal/engine/ipc).
//
// Concurrency: Run owns all session state in a single goroutine (the
// loop). Stop and Extend are commands answered by the loop; Snapshot and
// Subscribe read a mutex-guarded copy. Notifications are sent by a separate
// sender goroutine so a slow backend never delays a poll or a sleep.
//
// The loop never blocks except in one place, where it waits for the next
// deadline (poll, lease renew, grace end, recovery re-enable, session end,
// cap warning, heartbeat), a command, lease loss, or ctx. Deps.OnIdle is
// called right before that wait so a fake-clock driver (tests, `backpack
// simulate`) can advance time deterministically.
package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ilias-t/bckp/internal/config"
	"github.com/ilias-t/bckp/internal/contracts"
	"github.com/ilias-t/bckp/internal/humanize"
	"github.com/ilias-t/bckp/internal/paths"
	"github.com/ilias-t/bckp/internal/proto"
)

// Timings that are not user-configurable.
const (
	// RecoveryToggleGap is the pause between disabling and re-enabling
	// the hotspot service during a recovery.
	RecoveryToggleGap = 3 * time.Second
	// FlushTimeout bounds the notification flush before sleeping or
	// exiting.
	FlushTimeout = 5 * time.Second

	// The lease has a proto.DaemonDefaultTTL (90s) TTL and the loop renews
	// it every proto.RenewInterval (30s); nothing else renews it, so a hung
	// loop loses the lease within the TTL (docs/DESIGN.md dead-man's
	// switch, layer 3). A lease op that times out drops the lease
	// connection (the daemon then releases), so leaseTimeout is generous
	// but well under the 60s a lease has left when it is renewed.
	leaseTimeout = 15 * time.Second
	// renewRetry is the wait after a renew the daemon answered with an
	// error that kept the lease: several retries fit before it expires.
	renewRetry = 10 * time.Second

	providerTimeout = 10 * time.Second
	// netSnapshotTimeout bounds a network probe. It is longer than
	// providerTimeout because a probe that fails (up to 8s when DNS hangs
	// both HTTP targets) then runs the DNS/IP checks (about 2s more); cut
	// short, the checks would be skipped exactly when DNS is the problem.
	// It stays under the default 15s poll.
	netSnapshotTimeout = 14 * time.Second
	cleanupTimeout     = 20 * time.Second
)

// Deps are the engine's collaborators. Clock, Thermal, Power, Notifier and
// Guard are required; Network is required unless the network feature is
// off (config network.watchdog=false and no hotspot preference).
type Deps struct {
	Clock   contracts.Clock
	Thermal contracts.ThermalSource
	// Power errors matching contracts.ErrNoBattery turn the power rails
	// off for the session instead of counting as failures.
	Power   contracts.PowerSource
	Network contracts.Network
	// Notifier delivers pushed notifications. Errors matching
	// contracts.ErrQueued are recorded as NotificationQueued. When it
	// implements contracts.Outbox, the snapshot reports its depth and the
	// loop retries it while online.
	Notifier contracts.Notifier
	Guard    contracts.SleepGuard
	Config   config.Config
	Paths    paths.User
	// Logger receives the structured event log (see events.go and
	// internal/eventlog/events.md); wire it to eventlog.Open. It defaults
	// to a discarding logger. Tests and `backpack simulate` read the
	// timeline back from it.
	Logger *slog.Logger

	// LoadAvg returns the 1-minute load average for status lines. nil uses
	// sysctl vm.loadavg.
	LoadAvg func() (float64, error)
	// OnIdle, when set, is called from the engine goroutine right before
	// it blocks until next (its timer deadline, always after Clock.Now())
	// or a command. Tests and `backpack simulate` use it to drive a fake
	// clock. It must not call back into the engine synchronously.
	OnIdle func(next time.Time)
}

// Options are per-session settings from the command line. The hotspot
// preference and watchdog come from Deps.Config (the CLI writes its
// choices there).
type Options struct {
	// Duration is the requested session length. Zero or less means
	// forever: a bare `backpack` runs until stopped, capped by
	// max_duration.
	Duration time.Duration
	// Choices are the session options as the CLI resolved them (value and
	// source), reported unchanged in Snapshot.Options.
	Choices []contracts.SessionOption
}

// Engine runs one backpack session. Create it with New and call Run once.
type Engine struct {
	d     Deps
	cfg   config.Config
	log   *slog.Logger
	clock contracts.Clock
	pid   int
	// ob is Deps.Notifier as a contracts.Outbox, or nil.
	ob contracts.Outbox

	thermalRules thermalRules
	powerRules   powerRules
	netRules     networkRules

	started atomic.Bool
	cmds    chan command
	done    chan struct{}

	mu        sync.Mutex // guards the fields below
	snap      contracts.Snapshot
	subs      map[*subscriber]struct{}
	ended     bool
	recs      []notifRecord
	notifyErr string

	q *sendQueue

	// Loop-owned state.
	sess        contracts.SessionStatus
	choices     []contracts.SessionOption
	hotspot     config.HotspotPref
	mode        contracts.NetworkMode
	lease       contracts.Lease
	lost        <-chan error
	lastRenew   time.Time // for the event log
	th          ThermalFSM
	pw          PowerFSM
	nw          NetFSM
	lastThermal *contracts.ThermalReading
	lastPower   *contracts.PowerReading
	errs        map[string]string
	journal     *Journal
	status      string // latest status line
	noBattery   bool

	// Event-log bookkeeping (events.go).
	nextSample time.Time
	lastPollAt time.Time
	provErrs   map[string]*providerErr
	lastDiag   string
	recoverWhy string
	// nextOutboxFlush rate-limits outbox retries.
	nextOutboxFlush time.Time
	// held are notifications waiting for the next poll (Action.NextPoll).
	held []contracts.Notification

	nextPoll, nextRenew, nextHeartbeat time.Time
	toggle                             toggles
	timerCh                            <-chan time.Time
	timerAt                            time.Time

	endReason   contracts.EndReason
	endMsg      string
	endErr      error
	sleepAfter  bool
	sleepReason string
}

// toggles are the two-phase network changes in flight (netops.go). The
// second phase runs from the loop, so commands are served in between.
type toggles struct {
	// reenableAt is when a recovery turns the hotspot service back on;
	// reenableFailed: that already failed once.
	reenableAt     time.Time
	reenableFailed bool
	// nudgeOnAt is when a Wi-Fi nudge turns Wi-Fi back on; nudgeOnFailed:
	// that already failed once; nudgeOffErr is the off phase's error.
	nudgeOnAt     time.Time
	nudgeOnFailed bool
	nudgeOffErr   error
}

type notifRecord struct {
	id  uint64
	rec contracts.NotificationRecord
}

type cmdKind int

const (
	cmdStop cmdKind = iota
	cmdExtend
	cmdAbortSleep
)

type command struct {
	kind  cmdKind
	sleep *bool
	// signal names the signal behind a stop ("" for a user stop).
	signal  string
	minutes int
	forever bool
	reply   chan cmdReply
}

type cmdReply struct {
	snap contracts.Snapshot
	err  error
}

type subscriber struct {
	ch   chan contracts.Snapshot
	gone chan struct{}
}

// New returns an engine for one session.
func New(d Deps) *Engine {
	log := d.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	clock := d.Clock
	if clock == nil {
		clock = wallClock{}
	}
	if d.LoadAvg == nil {
		d.LoadAvg = sysLoadAvg
	}
	cfg := d.Config
	e := &Engine{
		d:            d,
		cfg:          cfg,
		log:          log,
		clock:        clock,
		pid:          os.Getpid(),
		thermalRules: thermalRules{Cfg: cfg.Thermal, Poll: cfg.Session.Poll},
		powerRules:   powerRules{Cfg: cfg.Power},
		cmds:         make(chan command),
		done:         make(chan struct{}),
		subs:         map[*subscriber]struct{}{},
		th:           NewThermalFSM(),
		errs:         map[string]string{},
		provErrs:     map[string]*providerErr{},
	}
	e.ob, _ = d.Notifier.(contracts.Outbox)
	e.snap = contracts.Snapshot{PID: e.pid, Thermal: contracts.ThermalStatus{FSM: contracts.FSMNominal}}
	return e
}

type wallClock struct{}

func (wallClock) Now() time.Time                         { return time.Now() }
func (wallClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// Done is closed once Run has returned and the final snapshot is out.
func (e *Engine) Done() <-chan struct{} { return e.done }

// Run acquires the sleep lease and runs the session until it ends: stop,
// expiry, max_duration, battery floor, thermal sleep, lease loss, or ctx
// cancellation (EndSignal). On every exit path, including a panic in the
// loop, it restores the network, flushes notifications, and releases the
// lease (or puts the Mac to sleep when the session ended in a sleep).
//
// It returns an error when the lease cannot be acquired, when the lease is
// lost, and on an engine panic; a normal end returns nil.
func (e *Engine) Run(ctx context.Context, opts Options) (err error) {
	if !e.started.CompareAndSwap(false, true) {
		return errors.New("engine: Run called twice")
	}
	defer close(e.done)
	if err := e.checkDeps(); err != nil {
		e.ev(slog.LevelError, "engine.error", "err", err)
		e.fail(err)
		return err
	}
	now := e.clock.Now()
	e.initSession(now, opts)
	e.logStart(opts)
	e.publish(now)

	actx, cancel := context.WithTimeout(ctx, leaseTimeout)
	lease, err := e.d.Guard.Acquire(actx, proto.DaemonDefaultTTL, e.leaseReason())
	cancel()
	if err != nil {
		err = fmt.Errorf("engine: acquire sleep lease: %w", err)
		e.ev(slog.LevelError, "lease.acquire_fail", "err", err)
		e.fail(err)
		return err
	}
	e.lease, e.lost, e.lastRenew = lease, lease.Lost(), now
	e.nextRenew = now.Add(proto.RenewInterval)
	e.ev(slog.LevelInfo, "lease.acquire", "lease", lease.ID(), "ttl", dur(proto.DaemonDefaultTTL), "expires", lease.Expires())

	e.q = newSendQueue(e)
	defer func() {
		if r := recover(); r != nil {
			e.ev(slog.LevelError, "engine.panic", "panic", fmt.Sprint(r), "stack", string(debug.Stack()))
			e.end(contracts.EndError, fmt.Sprintf("panic: %v", r))
			e.errs["engine"] = fmt.Sprintf("panic: %v", r)
			err = fmt.Errorf("engine: panic: %v", r)
		}
		e.shutdown()
		if err == nil {
			err = e.endErr
		}
	}()

	e.setupNetwork(ctx, now)
	// The start notification carries the status line, so it goes out after
	// the first poll has read the thermal, power and network state.
	e.poll(ctx, now)
	if !e.ending() {
		e.notifyStart(now)
	}
	e.loop(ctx)
	return nil
}

func (e *Engine) checkDeps() error {
	var missing []string
	if e.d.Thermal == nil {
		missing = append(missing, "Thermal")
	}
	if e.d.Power == nil {
		missing = append(missing, "Power")
	}
	if e.d.Notifier == nil {
		missing = append(missing, "Notifier")
	}
	if e.d.Guard == nil {
		missing = append(missing, "Guard")
	}
	if e.networkMode() != contracts.NetworkOff && e.d.Network == nil {
		missing = append(missing, "Network")
	}
	if len(missing) > 0 {
		return fmt.Errorf("engine: missing dependencies: %s", strings.Join(missing, ", "))
	}
	if err := e.cfg.Validate(); err != nil {
		return fmt.Errorf("engine: invalid config: %w", err)
	}
	return nil
}

// fail ends a session that never got its lease.
func (e *Engine) fail(err error) {
	now := e.clock.Now()
	e.endReason, e.endMsg = contracts.EndError, err.Error()
	e.errs["engine"] = err.Error()
	e.finish(now)
}

// networkMode maps network.prefer_hotspot and network.watchdog to the
// starting mode. auto and on probe the network even with the watchdog off
// (networkRules.Step only recovers when Cfg.Watchdog is set).
func (e *Engine) networkMode() contracts.NetworkMode {
	switch e.cfg.Network.PreferHotspot {
	case config.HotspotOn:
		return contracts.NetworkPreferHotspot
	case config.HotspotAuto:
		return contracts.NetworkHotspotAuto
	}
	return e.fallbackMode()
}

// fallbackMode is the mode without the hotspot preference.
func (e *Engine) fallbackMode() contracts.NetworkMode {
	if e.cfg.Network.Watchdog {
		return contracts.NetworkWatchdog
	}
	return contracts.NetworkOff
}

func (e *Engine) initSession(now time.Time, opts Options) {
	forever := opts.Duration <= 0
	e.sess = contracts.SessionStatus{StartedAt: now, Forever: forever, CapAt: now.Add(e.cfg.Session.MaxDuration)}
	if !forever {
		e.sess.EndsAt = now.Add(opts.Duration)
	}
	e.hotspot = e.cfg.Network.PreferHotspot
	e.mode = e.networkMode()
	e.choices = append([]contracts.SessionOption(nil), opts.Choices...)
	e.netRules = networkRules{Cfg: e.cfg.Network, Poll: e.cfg.Session.Poll, Mode: e.mode, OutageMin: e.cfg.Notify.OutageMin}
	e.nextPoll = now
	if hb := e.cfg.Notify.Heartbeat; hb > 0 {
		e.nextHeartbeat = now.Add(hb)
	}
}

func (e *Engine) leaseReason() string {
	now := e.clock.Now()
	if e.sess.Forever {
		return fmt.Sprintf("backpack pid %d, forever (cap %s)", e.pid, humanize.Clock(e.sess.CapAt, now))
	}
	return fmt.Sprintf("backpack pid %d, until %s", e.pid, humanize.Clock(e.sess.EndsAt, now))
}

// effectiveEnd is the earlier of the requested end and the cap.
func (e *Engine) effectiveEnd() (time.Time, contracts.EndReason) {
	if !e.sess.Forever && !e.sess.EndsAt.After(e.sess.CapAt) {
		return e.sess.EndsAt, contracts.EndExpired
	}
	return e.sess.CapAt, contracts.EndMaxDuration
}

// capWarnAt returns when the cap warning is due, or zero when the cap does
// not end the session or the warning was sent.
func (e *Engine) capWarnAt() time.Time {
	if e.sess.CapWarned || e.cfg.Session.MaxDurationWarning <= 0 {
		return time.Time{}
	}
	if _, why := e.effectiveEnd(); why != contracts.EndMaxDuration {
		return time.Time{}
	}
	return e.sess.CapAt.Add(-e.cfg.Session.MaxDurationWarning)
}

func (e *Engine) ending() bool { return e.endReason != "" }

// end records the first end reason; later calls are ignored.
func (e *Engine) end(reason contracts.EndReason, msg string) {
	if e.endReason != "" {
		return
	}
	e.endReason, e.endMsg = reason, msg
	e.ev(slog.LevelDebug, "session.ending", "reason", string(reason), "detail", msg)
}

func (e *Engine) loop(ctx context.Context) {
	for {
		now := e.clock.Now()
		e.runDue(ctx, now)
		e.publish(now)
		if e.ending() {
			return
		}
		ch, at := e.timer(now)
		if e.d.OnIdle != nil {
			e.d.OnIdle(at)
		}
		select {
		case <-ctx.Done():
			e.end(contracts.EndSignal, "context cancelled")
		case <-ch:
			e.timerCh = nil
		case err, ok := <-e.lost:
			e.lost = nil
			if !ok || err == nil {
				err = contracts.ErrLeaseLost
			}
			e.leaseLost(e.clock.Now(), err)
		case c := <-e.cmds:
			e.handle(c)
		}
	}
}

// runDue handles every deadline at or before now.
func (e *Engine) runDue(ctx context.Context, now time.Time) {
	if e.ending() {
		return
	}
	if p := e.th.Pending; p != nil && !now.Before(p.At) {
		th, acts := e.thermalRules.Expire(e.th, now)
		e.setThermal(now, th, acts)
	}
	if t := e.toggle.reenableAt; !t.IsZero() && !now.Before(t) {
		e.finishRecovery(ctx, now)
	}
	if t := e.toggle.nudgeOnAt; !t.IsZero() && !now.Before(t) {
		e.finishWiFiNudge(ctx, now)
	}
	if e.ending() {
		return
	}
	if !now.Before(e.nextRenew) {
		e.renew(ctx, now)
	}
	if end, why := e.effectiveEnd(); !now.Before(end) {
		msg := "requested duration elapsed"
		if why == contracts.EndMaxDuration {
			msg = fmt.Sprintf("max_duration %s reached", humanize.Duration(e.cfg.Session.MaxDuration))
		}
		e.end(why, msg)
		return
	}
	if at := e.capWarnAt(); !at.IsZero() && !now.Before(at) {
		e.sess.CapWarned = true
		e.send(now, notifyAttention(contracts.PriorityHigh,
			fmt.Sprintf("backpack ends in %s (%s cap)", humanize.Duration(e.sess.CapAt.Sub(now)), humanize.Duration(e.cfg.Session.MaxDuration)),
			fmt.Sprintf("Cap at %s. `bckp extend 60` or `bckp extend forever` resets it. %s", humanize.Clock(e.sess.CapAt, now), e.status),
			"hourglass").Notification)
	}
	if !now.Before(e.nextPoll) {
		e.poll(ctx, now)
		if e.ending() {
			return
		}
	}
	if !e.nextHeartbeat.IsZero() && !now.Before(e.nextHeartbeat) {
		e.nextHeartbeat = now.Add(e.cfg.Notify.Heartbeat)
		e.heartbeat(now)
	}
}

// timer returns a channel that fires at the earliest pending deadline,
// reusing the current timer when it fires no later than that (a fake clock
// never drops timers, so creating one per wake-up would pile them up).
func (e *Engine) timer(now time.Time) (<-chan time.Time, time.Time) {
	at := e.nextDeadline()
	if !at.After(now) {
		at = now.Add(time.Millisecond)
	}
	if e.timerCh != nil && !e.timerAt.After(at) && e.timerAt.After(now) {
		return e.timerCh, e.timerAt
	}
	e.timerCh, e.timerAt = e.clock.After(at.Sub(now)), at
	return e.timerCh, at
}

func (e *Engine) nextDeadline() time.Time {
	end, _ := e.effectiveEnd()
	next := minTime(e.nextPoll, e.nextRenew, end, e.capWarnAt(), e.nextHeartbeat, e.toggle.reenableAt, e.toggle.nudgeOnAt)
	if e.th.Pending != nil {
		next = minTime(next, e.th.Pending.At)
	}
	return next
}

// minTime returns the earliest non-zero time.
func minTime(ts ...time.Time) time.Time {
	var m time.Time
	for _, t := range ts {
		if !t.IsZero() && (m.IsZero() || t.Before(m)) {
			m = t
		}
	}
	return m
}

// renew is the only lease renewal. An error that keeps the lease (the
// daemon answered, but not with success) is retried after renewRetry;
// anything else ends the session.
func (e *Engine) renew(ctx context.Context, now time.Time) {
	rctx, cancel := context.WithTimeout(ctx, leaseTimeout)
	defer cancel()
	if err := e.lease.Renew(rctx); err != nil {
		if errors.Is(err, contracts.ErrLeaseLost) {
			e.leaseLost(now, err)
			return
		}
		e.nextRenew = now.Add(renewRetry)
		e.errs["lease"] = "renew: " + err.Error()
		e.ev(slog.LevelWarn, "lease.renew_fail", "lease", e.lease.ID(), "err", err, "last_renew", e.lastRenew)
		return
	}
	e.nextRenew = now.Add(proto.RenewInterval)
	delete(e.errs, "lease")
	e.lastRenew = now
}

func (e *Engine) leaseLost(now time.Time, err error) {
	if e.ending() {
		return
	}
	e.errs["lease"] = err.Error()
	e.endErr = fmt.Errorf("engine: %w", err)
	e.ev(slog.LevelError, "lease.lost", "lease", e.lease.ID(), "err", err, "last_renew", e.lastRenew)
	e.send(now, notifyAttention(contracts.PriorityUrgent, "Sleep guard lost - backpack off",
		fmt.Sprintf("The daemon lease ended (%v). The Mac sleeps normally again when the lid is closed. Start a new session with `bckp`.", err),
		"warning", "zzz").Notification)
	e.end(contracts.EndLeaseLost, err.Error())
}

func (e *Engine) handle(c command) {
	now := e.clock.Now()
	pendingCritical := e.th.Pending != nil && !e.th.Pending.Abortable
	switch c.kind {
	case cmdStop:
		sleepArg := "default"
		if c.sleep != nil {
			sleepArg = fmt.Sprint(*c.sleep)
		}
		if c.sleep != nil && !*c.sleep && pendingCritical {
			e.ev(slog.LevelWarn, "session.stop", "sleep", sleepArg, "result", "refused: critical sleep pending")
			c.reply <- cmdReply{err: contracts.ErrNotAbortable}
			return
		}
		args := []any{"sleep", sleepArg, "sleep_pending", e.th.Pending != nil}
		if c.signal != "" {
			args = append(args, "signal", c.signal)
		}
		e.ev(slog.LevelInfo, "session.stop", args...)
		switch {
		case pendingCritical:
			p := e.th.Pending
			th := e.th.clone()
			th.State, th.Pending = contracts.FSMSleeping, nil
			e.setThermal(now, th, []Action{{Kind: ActSleepNow, Trigger: p.Trigger, Msg: p.Reason + " (stop requested)"}})
		case c.sleep != nil && *c.sleep:
			e.sleepAfter, e.sleepReason = true, "backpack stop --sleep"
			e.end(contracts.EndUserSleep, "stop --sleep")
		default:
			reason, msg := contracts.EndStopped, "stop requested"
			if c.signal != "" {
				reason, msg = contracts.EndSignal, c.signal+" received"
			}
			if e.th.Pending != nil {
				msg += "; pending sleep cancelled"
			}
			e.end(reason, msg)
		}
		c.reply <- cmdReply{}

	case cmdExtend:
		if !c.forever && (c.minutes < 1 || c.minutes > proto.MaxExtendMinutes) {
			c.reply <- cmdReply{err: fmt.Errorf("engine: extend minutes must be 1..%d", proto.MaxExtendMinutes)}
			return
		}
		if pendingCritical {
			c.reply <- cmdReply{err: contracts.ErrNotAbortable}
			return
		}
		th, acts, err := e.thermalRules.Abort(e.th, "extend", now)
		if err != nil {
			c.reply <- cmdReply{err: err}
			return
		}
		e.setThermal(now, th, acts)
		var desc string
		switch {
		case c.forever:
			e.sess.Forever, e.sess.EndsAt = true, time.Time{}
			desc = "forever"
		case e.sess.Forever:
			desc = "(forever session)" // minutes only reset the cap
		default:
			base := e.sess.EndsAt
			if base.Before(now) {
				base = now
			}
			e.sess.EndsAt = base.Add(time.Duration(c.minutes) * time.Minute)
			desc = "until " + humanize.Clock(e.sess.EndsAt, now)
		}
		e.sess.CapAt = now.Add(e.cfg.Session.MaxDuration)
		e.sess.CapWarned = false
		e.ev(slog.LevelInfo, "session.extend", "minutes", c.minutes, "forever", c.forever, "desc", desc,
			"cap_at", e.sess.CapAt.Format(time.RFC3339))
		e.send(now, contracts.Notification{Priority: contracts.PriorityLow, Title: "backpack extended " + desc,
			Body: fmt.Sprintf("Cap reset to %s. %s", humanize.Clock(e.sess.CapAt, now), e.status), Tags: []string{"hourglass"},
			Class: contracts.ClassInfo})
		snap := e.publish(now)
		c.reply <- cmdReply{snap: snap}

	case cmdAbortSleep:
		switch {
		case e.th.Pending == nil:
			c.reply <- cmdReply{err: contracts.ErrNoSleepPending}
			return
		case pendingCritical:
			c.reply <- cmdReply{err: contracts.ErrNotAbortable}
			return
		}
		th, acts, err := e.thermalRules.Abort(e.th, "abort", now)
		if err != nil {
			c.reply <- cmdReply{err: err}
			return
		}
		e.setThermal(now, th, acts)
		c.reply <- cmdReply{snap: e.publish(now)}
	}
}

func (e *Engine) poll(ctx context.Context, now time.Time) {
	e.nextPoll = now.Add(e.cfg.Session.Poll)
	e.suspectSleep(now)
	e.sendHeld(now)

	// The providers are read concurrently, so a poll takes as long as the
	// slowest one (the network probe, up to netSnapshotTimeout), not their
	// sum.
	var (
		tr   contracts.ThermalReading
		pr   contracts.PowerReading
		ns   contracts.NetSnapshot
		terr error
		perr error
		nerr error
	)
	probe := e.mode != contracts.NetworkOff
	readAll(
		func() {
			rctx, cancel := context.WithTimeout(ctx, providerTimeout)
			defer cancel()
			tr, terr = e.d.Thermal.Read(rctx)
		},
		func() {
			rctx, cancel := context.WithTimeout(ctx, providerTimeout)
			defer cancel()
			pr, perr = e.d.Power.Read(rctx)
		},
		func() {
			if !probe {
				return
			}
			rctx, cancel := context.WithTimeout(ctx, netSnapshotTimeout)
			defer cancel()
			ns, nerr = e.d.Network.Snapshot(rctx)
		},
	)

	if terr == nil {
		if tr.At.IsZero() {
			tr.At = now
		}
		e.lastThermal = &tr
	}
	setErr(e.errs, "thermal", terr)
	e.providerResult(now, "thermal", terr)

	noBattery := errors.Is(perr, contracts.ErrNoBattery)
	if noBattery && !e.noBattery {
		e.noBattery = true
		e.ev(slog.LevelInfo, "power.no_battery", "err", perr, "note", "power rails off for this session")
	}
	if perr == nil {
		if pr.At.IsZero() {
			pr.At = now
		}
		e.lastPower = &pr
	}
	if !noBattery {
		setErr(e.errs, "power", perr)
		e.providerResult(now, "power", perr)
	}

	if probe {
		if nerr == nil && ns.At.IsZero() {
			ns.At = now
		}
		setErr(e.errs, "network", nerr)
		e.providerResult(now, "network", nerr)
	}

	e.status = e.statusLine(now)
	th, acts := e.thermalRules.Step(e.th, ThermalInput{Reading: tr, Err: terr, Status: e.status}, now)
	e.setThermal(now, th, acts)
	if e.ending() {
		return
	}

	if !noBattery {
		pw, pacts := e.powerRules.Step(e.pw, PowerInput{Reading: pr, Err: perr, Status: e.status}, now)
		if perr == nil && (!e.pw.Known || e.pw.OnAC != pw.OnAC) {
			from := "unknown"
			if e.pw.Known {
				from = powerSrc(e.pw.OnAC)
			}
			e.ev(slog.LevelInfo, "power.transition", "from", from, "to", powerSrc(pr.OnAC), "battery", pr.Percent,
				"charging", pr.Charging, "lid", lidState(pr.LidClosed), "sleep_disabled", pr.SleepDisabled)
		}
		e.pw = pw
		e.apply(ctx, now, "power", pacts)
		if e.ending() {
			return
		}
	}

	// Without a probe, whether the Mac is online is unknown: the outbox is
	// retried anyway (a flush while offline fails fast).
	online := !probe
	if probe {
		nw, nacts := e.netRules.Step(e.nw, NetInput{Snap: ns, Err: nerr}, now)
		e.setNet(ctx, now, nw, nacts)
		if e.ending() {
			return
		}
		online = nerr == nil && ns.Online
	}
	if online {
		e.retryOutbox(now)
	}
	e.sample(now)
}

// readAll runs fns concurrently and waits for all of them. A panic in one
// is re-raised on the caller's goroutine, where Run recovers it as an
// engine error.
func readAll(fns ...func()) {
	panics := make([]any, len(fns))
	var wg sync.WaitGroup
	for i, fn := range fns {
		wg.Go(func() {
			defer func() { panics[i] = recover() }()
			fn()
		})
	}
	wg.Wait()
	for _, p := range panics {
		if p != nil {
			panic(p)
		}
	}
}

func setErr(errs map[string]string, key string, err error) {
	if err != nil {
		errs[key] = err.Error()
	} else {
		delete(errs, key)
	}
}

func (e *Engine) setThermal(now time.Time, th ThermalFSM, acts []Action) {
	if th.State != e.th.State {
		args := append([]any{"from", string(e.th.State), "to", string(th.State)}, thermalAttrs(e.lastThermal)...)
		if !th.HotSince.IsZero() {
			args = append(args, "hot_for", dur(now.Sub(th.HotSince)))
		}
		e.ev(slog.LevelInfo, "thermal.transition", args...)
	}
	e.th = th
	e.apply(context.Background(), now, "thermal", acts)
}

func (e *Engine) setNet(ctx context.Context, now time.Time, nw NetFSM, acts []Action) {
	e.noteNetwork(now, e.nw, nw)
	e.nw = nw
	e.apply(ctx, now, "net", acts)
}

// apply carries out state-machine actions. src names the state machine
// ("thermal", "power", "net") for its <src>.note log records.
func (e *Engine) apply(ctx context.Context, now time.Time, src string, acts []Action) {
	for _, a := range acts {
		switch a.Kind {
		case ActNotify:
			if a.NextPoll {
				e.held = append(e.held, a.Notification)
				e.ev(slog.LevelDebug, "notify.held", "title", a.Notification.Title, "until", "next poll")
				continue
			}
			e.send(now, a.Notification)
		case ActLog:
			e.log.Log(context.Background(), slog.LevelDebug, a.Msg, "event", src+".note")
		case ActSleepPending:
			args := []any{"trigger", string(a.Trigger), "reason", a.Msg}
			if p := e.th.Pending; p != nil {
				args = append(args, "at", p.At, "abortable", p.Abortable)
			}
			e.ev(slog.LevelWarn, "sleep.pending", append(args, thermalAttrs(e.lastThermal)...)...)
		case ActSleepCancel:
			e.ev(slog.LevelInfo, "sleep.cancel", "trigger", string(a.Trigger), "by", a.Msg)
		case ActSleepNow:
			e.ev(slog.LevelWarn, "sleep.now", "trigger", string(a.Trigger), "reason", a.Msg)
			e.sleepAfter = true
			e.sleepReason = fmt.Sprintf("thermal %s: %s", a.Trigger, a.Msg)
			e.end(contracts.EndThermalSleep, e.sleepReason)
		case ActEndSession:
			e.end(a.EndReason, a.Msg)
		case ActRecover:
			e.startRecovery(ctx, now, a.Msg)
		case ActPreferHotspot:
			e.engageHotspot(ctx, now, a.Msg)
		case ActWiFiNudge:
			e.startWiFiNudge(ctx, now, a.Msg)
		}
	}
}

// shutdown runs on every exit path after the lease was acquired. Each step
// runs with a fresh context (the session ctx may be cancelled) and its own
// panic guard, so a failure in one never skips the lease release.
func (e *Engine) shutdown() {
	now := e.clock.Now()
	if e.endReason == "" {
		e.end(contracts.EndError, "loop exited without a reason")
	}
	if e.th.State == contracts.FSMSleepPending && !e.sleepAfter {
		e.th.State, e.th.Pending = contracts.FSMHot, nil
		if e.th.Last < contracts.ThermalSerious {
			e.th.State = contracts.FSMFair
		}
	}
	e.q.stopFlushes()
	e.step("restore network", e.restoreNetwork)
	e.step("stop notification", func() {
		e.sendHeld(now)
		e.status = e.statusLine(now)
		e.notifyStop(now)
	})
	e.step("flush notifications", e.flushNotifications)
	slept := false
	if e.sleepAfter {
		e.step("sleep", func() {
			ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
			defer cancel()
			e.ev(slog.LevelWarn, "sleep.request", "reason", e.sleepReason)
			if err := e.d.Guard.SleepNow(ctx, e.sleepReason); err != nil {
				e.errs["lease"] = "sleep: " + err.Error()
				e.ev(slog.LevelError, "sleep.failed", "err", err, "fallback", "release lease")
				e.send(now, contracts.Notification{Priority: contracts.PriorityUrgent, Title: "Sleep failed - lease released", Class: contracts.ClassAttention,
					Body: fmt.Sprintf("backpackd could not sleep the Mac (%v). The lease is released, so a closed lid sleeps it.", err), Tags: []string{"warning"}})
				e.flushNotifications()
				return
			}
			slept = true
		})
	}
	if !slept {
		e.step("release lease", func() {
			ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
			defer cancel()
			if err := e.lease.Release(ctx); err != nil {
				e.errs["lease"] = "release: " + err.Error()
				e.ev(slog.LevelError, "lease.release", "lease", e.lease.ID(), "result", "failed", "err", err)
				return
			}
			e.ev(slog.LevelInfo, "lease.release", "lease", e.lease.ID(), "result", "ok")
		})
	}
	e.q.close()
	e.finish(now)
}

func (e *Engine) step(name string, fn func()) {
	defer func() {
		if r := recover(); r != nil {
			e.errs["engine"] = fmt.Sprintf("%s: panic: %v", name, r)
			e.ev(slog.LevelError, "engine.panic", "step", name, "panic", fmt.Sprint(r), "stack", string(debug.Stack()))
		}
	}()
	fn()
}

// finish publishes the final snapshot and closes all subscriptions.
func (e *Engine) finish(now time.Time) {
	e.sess.Ended, e.sess.EndReason = true, e.endReason
	if e.endReason != contracts.EndThermalSleep {
		e.th.Pending = nil
	}
	snap := e.buildSnapshot(now)
	snap.Lease = contracts.LeaseStatus{}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.attachLocked(&snap)
	e.snap = snap
	e.ended = true
	for sub := range e.subs {
		offer(sub.ch, snap)
		close(sub.ch)
		close(sub.gone)
		delete(e.subs, sub)
	}
	args := []any{"reason", string(e.endReason), "detail", e.endMsg}
	if !e.sess.StartedAt.IsZero() {
		args = append(args, "ran", dur(now.Sub(e.sess.StartedAt)))
	}
	args = append(args, "slept", e.sleepAfter && !strings.HasPrefix(e.errs["lease"], "sleep:"))
	if len(e.errs) > 0 {
		errs := make([]any, 0, len(e.errs)*2)
		for k, v := range e.errs {
			errs = append(errs, k, v)
		}
		args = append(args, slog.Group("errors", errs...))
	}
	e.ev(slog.LevelInfo, "session.end", args...)
}

// Stop ends the session; sleep is tri-state (see proto.EngineRequest):
//
//   - nil: a pending critical sleep executes now (EndThermalSleep); a
//     pending abortable sleep is cancelled (EndStopped).
//   - true: end the session and sleep (EndUserSleep; EndThermalSleep if a
//     critical sleep was pending).
//   - false: end without sleeping; contracts.ErrNotAbortable (and no
//     change) while a critical sleep is pending.
//
// Stop returns after the session has fully ended (lease released or Mac
// asleep, final snapshot published), so Snapshot then has Session.Ended.
// It returns nil when the session already ended and
// contracts.ErrEngineNotRunning before Run.
func (e *Engine) Stop(ctx context.Context, sleep *bool) error {
	return e.stop(ctx, command{kind: cmdStop, sleep: sleep})
}

// StopForSignal is Stop(ctx, nil) for a stop nobody asked for from
// backpack: a SIGTERM or SIGHUP (logout, kill, a closed terminal). The
// session ends as EndSignal, which needs attention, instead of
// EndStopped. name is the signal ("SIGTERM").
func (e *Engine) StopForSignal(ctx context.Context, name string) error {
	return e.stop(ctx, command{kind: cmdStop, signal: name})
}

func (e *Engine) stop(ctx context.Context, c command) error {
	_, err := e.command(ctx, c)
	if err != nil {
		return err
	}
	// A finished session wins over an expired ctx: select picks randomly
	// among ready cases, so check done again once ctx has fired.
	select {
	case <-e.done:
		return nil
	case <-ctx.Done():
		if isClosed(e.done) {
			return nil
		}
		return ctx.Err()
	}
}

// AbortSleep cancels a pending abortable sleep without touching the
// session end or the cap. It returns contracts.ErrNoSleepPending when no
// sleep is pending and contracts.ErrNotAbortable for a critical one.
func (e *Engine) AbortSleep(ctx context.Context) (contracts.Snapshot, error) {
	return e.command(ctx, command{kind: cmdAbortSleep})
}

// isClosed reports whether ch is closed (it is never sent on).
func isClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// Extend adds minutes to the requested end (from now if it has passed; a
// forever session stays forever) or switches to forever. Both reset the
// cap to now + max_duration, clear the cap warning, and cancel a pending
// abortable sleep. With a critical sleep pending it returns
// contracts.ErrNotAbortable and changes nothing. It returns the updated
// snapshot.
func (e *Engine) Extend(ctx context.Context, minutes int, forever bool) (contracts.Snapshot, error) {
	return e.command(ctx, command{kind: cmdExtend, minutes: minutes, forever: forever})
}

func (e *Engine) command(ctx context.Context, c command) (contracts.Snapshot, error) {
	if !e.started.Load() {
		return contracts.Snapshot{}, contracts.ErrEngineNotRunning
	}
	c.reply = make(chan cmdReply, 1)
	ended := func() (contracts.Snapshot, error) {
		if c.kind == cmdStop {
			return e.Snapshot(), nil
		}
		return contracts.Snapshot{}, contracts.ErrEngineNotRunning
	}
	// Go picks randomly among ready select cases, so an ended session or
	// an answered command is re-checked before reporting ctx's error: a
	// stop that already happened must not come back as "context canceled".
	if isClosed(e.done) {
		return ended()
	}
	select {
	case e.cmds <- c:
	case <-e.done:
		return ended()
	case <-ctx.Done():
		if isClosed(e.done) {
			return ended()
		}
		return contracts.Snapshot{}, ctx.Err()
	}
	select {
	case r := <-c.reply:
		return r.snap, r.err
	case <-e.done:
		// A panic in the loop can end Run without an answer.
	case <-ctx.Done():
	}
	select {
	case r := <-c.reply:
		return r.snap, r.err
	default:
	}
	if isClosed(e.done) {
		return ended()
	}
	return contracts.Snapshot{}, ctx.Err()
}
