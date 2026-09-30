package engine

import (
	"fmt"
	"time"

	"github.com/ilias-t/bckp/internal/config"
	"github.com/ilias-t/bckp/internal/contracts"
	"github.com/ilias-t/bckp/internal/humanize"
)

// Thermal state machine (docs/DESIGN.md "State machines").
//
//	NOMINAL        -> FAIR           after FairPolls fair polls in a row
//	                                 (notify p3, at most once per FairCooldown)
//	NOMINAL|FAIR   -> HOT            on any serious or critical poll
//	                                 (notify p4, or p5 for critical)
//	HOT            -> HOT            re-notify every HotRenotify; escalate to
//	                                 p5 immediately on the first critical poll
//	HOT            -> FAIR           after CooledPolls polls below serious
//	                                 ("cooled down"; the hot timer resets)
//	FAIR           -> NOMINAL        after NominalPolls nominal polls
//	                                 ("back to nominal", only if the episode
//	                                 was notified)
//	HOT            -> SLEEP_PENDING  after SeriousSleepAfter of continuous
//	                                 HOT (abortable, SeriousGrace), or after
//	                                 CriticalPolls critical polls in a row
//	                                 (not abortable, CriticalGrace)
//	any            -> SLEEP_PENDING  optional cpu_sleep_c: CPU °C at or above
//	                                 the threshold for cpuSleepPolls polls
//	                                 (abortable, SeriousGrace)
//	any            -> SLEEP_PENDING  battery °C at or above battery_sleep_c
//	                                 for batteryPolls polls (abortable,
//	                                 SeriousGrace), or at or above
//	                                 battery_critical_c (not abortable,
//	                                 CriticalGrace)
//	SLEEP_PENDING  -> SLEEPING       when the grace period ends (Expire)
//
// An abortable pending sleep that sees CriticalPolls critical polls, or
// battery_critical_c for batteryPolls polls, is upgraded to a critical one
// (not abortable; the deadline moves earlier if CriticalGrace is shorter
// than what remains).
//
// Battery °C runs beside the states above because the thermal state can
// stay nominal while a closed bag heats the battery: the chip stays cool
// enough not to throttle. battery_warn_c for batteryPolls polls notifies
// once, re-armed after CooledPolls polls below it. An unreadable battery
// °C is notified once after thermalFailNotifyPolls polls.
//
// A failed reading (read error or ThermalUnknown) changes no counters and
// does not advance the hot timer: the interval that ends at the first good
// reading after a failure is not counted as hot time. After
// thermalFailNotifyPolls failures in a row the user is notified once.

const (
	// thermalFailNotifyPolls consecutive failed readings trigger a
	// "thermal readings failing" notification (1 minute at the default
	// 15s poll).
	thermalFailNotifyPolls = 4
	// cpuSleepPolls consecutive polls at or above cpu_sleep_c trigger an
	// abortable sleep. Two polls filter out single-sample spikes.
	cpuSleepPolls = 2
	// batteryPolls consecutive polls at or above a battery threshold
	// trigger its warning or sleep, filtering single-sample spikes.
	batteryPolls = 2
)

// thermalRules is the thermal state machine's configuration.
type thermalRules struct {
	Cfg config.Thermal
	// Poll is the poll interval. When set, one poll interval of hot time is
	// counted at most 2*Poll, so a long gap (Mac asleep, engine stalled)
	// is not mistaken for continuous heat.
	Poll time.Duration
}

// PendingSleep is a scheduled sleep in its grace period.
type PendingSleep struct {
	Trigger contracts.SleepTrigger
	Reason  string
	// At is when the grace period ends.
	At        time.Time
	Abortable bool
}

// ThermalFSM is the thermal state machine's state. The zero value is not
// valid; use NewThermalFSM.
type ThermalFSM struct {
	State contracts.FSMState
	// Last is the state of the latest good reading.
	Last contracts.ThermalState
	// Alerted is the highest level notified in the current hot episode
	// (ThermalSerious or ThermalCritical), ThermalUnknown outside one.
	Alerted contracts.ThermalState
	// FairNotified: the current FAIR episode was notified, so a "back to
	// nominal" notice is owed.
	FairNotified bool

	FairCount, CritCount, CalmCount, NominalCount, CPUHotCount int

	// Battery °C counters: polls at or above battery_warn_c,
	// battery_sleep_c and battery_critical_c, below battery_warn_c, and
	// without a battery reading.
	BattWarmCount, BattHotCount, BattCritCount, BattCalmCount, BattMissCount int
	// BattWarned: the battery warning was sent and is not yet re-armed.
	BattWarned bool
	// BattMissNotified: "battery temperature unavailable" was sent.
	BattMissNotified bool

	FailCount    int
	FailNotified bool

	// LastGood is when the latest good reading was taken.
	LastGood time.Time
	// HotSince is when the current hot episode started (zero outside).
	HotSince time.Time
	// HotFor is the hot time counted towards SeriousSleepAfter.
	HotFor time.Duration

	LastHotNotify  time.Time
	LastFairNotify time.Time
	// CPUQuietUntil suppresses the cpu_sleep_c and battery_sleep_c
	// triggers after an abort. battery_critical_c is never suppressed.
	CPUQuietUntil time.Time

	// Pending is set while State is SLEEP_PENDING.
	Pending *PendingSleep
}

// NewThermalFSM returns the initial state (NOMINAL).
func NewThermalFSM() ThermalFSM {
	return ThermalFSM{State: contracts.FSMNominal}
}

// ThermalInput is one thermal poll.
type ThermalInput struct {
	Reading contracts.ThermalReading
	// Err is the read error, if any.
	Err error
	// Status is a one-line machine status appended to notification bodies.
	Status string
}

// Step advances the machine by one poll. It is pure: s is not modified.
func (r thermalRules) Step(s ThermalFSM, in ThermalInput, now time.Time) (ThermalFSM, []Action) {
	s = s.clone()
	if s.State == contracts.FSMSleeping {
		return s, nil
	}
	s, acts := r.Expire(s, now)
	if s.State == contracts.FSMSleeping {
		return s, acts
	}
	c := r.Cfg
	st := in.Reading.State

	if in.Err != nil || st <= contracts.ThermalUnknown || st > contracts.ThermalCritical {
		s.FailCount++
		why := fmt.Sprintf("state %s", st)
		if in.Err != nil {
			why = in.Err.Error()
		}
		acts = append(acts, logf("thermal reading failed (%d in a row): %s", s.FailCount, why))
		if s.FailCount >= thermalFailNotifyPolls && !s.FailNotified {
			s.FailNotified = true
			acts = append(acts, notifyAttention(contracts.PriorityHigh, "Thermal readings failing",
				fmt.Sprintf("%d failed readings in a row (%s). Heat protection is blind; the daemon's critical-heat backstop still runs.", s.FailCount, why),
				"warning"))
		}
		return s, acts
	}

	if s.FailNotified {
		s.FailNotified = false
		acts = append(acts, notifyInfo(contracts.PriorityLow, "Thermal readings back",
			fmt.Sprintf("Thermal state is %s again. %s", st, in.Status), "white_check_mark"))
	}
	var interval time.Duration
	counted := s.FailCount == 0 && !s.LastGood.IsZero()
	if counted {
		interval = now.Sub(s.LastGood)
		if r.Poll > 0 && interval > 2*r.Poll {
			interval = 2 * r.Poll
		}
	}
	s.FailCount = 0
	s.LastGood = now
	s.Last = st

	s.FairCount = countIf(st == contracts.ThermalFair, s.FairCount)
	s.CritCount = countIf(st == contracts.ThermalCritical, s.CritCount)
	s.NominalCount = countIf(st == contracts.ThermalNominal, s.NominalCount)
	s.CalmCount = countIf(st < contracts.ThermalSerious, s.CalmCount)
	cpuHot := c.CPUSleepC != nil && in.Reading.CPUC != nil && *in.Reading.CPUC >= *c.CPUSleepC
	s.CPUHotCount = countIf(cpuHot, s.CPUHotCount)
	batt := in.Reading.BatteryC
	s.BattWarmCount = countIf(batt != nil && *batt >= c.BatteryWarnC, s.BattWarmCount)
	s.BattHotCount = countIf(batt != nil && *batt >= c.BatterySleepC, s.BattHotCount)
	s.BattCritCount = countIf(batt != nil && *batt >= c.BatteryCriticalC, s.BattCritCount)
	s.BattCalmCount = countIf(batt != nil && *batt < c.BatteryWarnC, s.BattCalmCount)
	s.BattMissCount = countIf(batt == nil, s.BattMissCount)
	acts = append(acts, r.batteryMissing(&s, in.Status)...)

	switch s.State {
	case contracts.FSMNominal, contracts.FSMFair:
		switch {
		case st >= contracts.ThermalSerious:
			acts = append(acts, r.enterHot(&s, st, now, in.Status))
		case s.State == contracts.FSMNominal && s.FairCount >= c.FairPolls:
			s.State = contracts.FSMFair
			if s.LastFairNotify.IsZero() || now.Sub(s.LastFairNotify) >= c.FairCooldown {
				s.FairNotified = true
				s.LastFairNotify = now
				acts = append(acts, notifyInfo(contracts.PriorityDefault, "Mac warming up (fair)", in.Status, "warning"))
			} else {
				s.FairNotified = false
				acts = append(acts, logf("fair notification suppressed (last one %s ago)", humanize.Duration(now.Sub(s.LastFairNotify))))
			}
		case s.State == contracts.FSMFair && s.NominalCount >= c.NominalPolls:
			s.State = contracts.FSMNominal
			if s.FairNotified {
				acts = append(acts, notifyInfo(contracts.PriorityLow, "Mac back to nominal", in.Status, "white_check_mark"))
			}
			s.FairNotified = false
		}

	case contracts.FSMHot:
		if st >= contracts.ThermalSerious {
			s.HotFor += min(interval, now.Sub(s.HotSince))
			// The pending-sleep notice replaces a HOT notice on the poll
			// that triggers it.
			pends := s.CritCount >= c.CriticalPolls || s.HotFor >= c.SeriousSleepAfter
			if a, ok := r.hotNotify(&s, st, now, in.Status, true); ok && !pends {
				acts = append(acts, a)
			}
		} else if s.CalmCount >= c.CooledPolls {
			hotFor := now.Sub(s.HotSince)
			s.HotSince, s.HotFor, s.Alerted = time.Time{}, 0, contracts.ThermalUnknown
			s.LastFairNotify = now
			if st == contracts.ThermalNominal && s.NominalCount >= c.NominalPolls {
				s.State, s.FairNotified = contracts.FSMNominal, false
			} else {
				s.State, s.FairNotified = contracts.FSMFair, true
			}
			acts = append(acts, notifyAttention(contracts.PriorityDefault, fmt.Sprintf("Mac cooled down (%s)", st),
				fmt.Sprintf("Hot for %s. %s", humanize.Duration(hotFor), in.Status), "white_check_mark"))
		}

	case contracts.FSMSleepPending:
		if st >= contracts.ThermalSerious && !s.HotSince.IsZero() {
			s.HotFor += min(interval, now.Sub(s.HotSince))
			if a, ok := r.hotNotify(&s, st, now, in.Status, false); ok {
				acts = append(acts, a)
			}
		}
	}

	// Sleep triggers.
	upgradable := s.State != contracts.FSMSleepPending || s.Pending.Abortable
	switch {
	case s.CritCount >= c.CriticalPolls && (s.State == contracts.FSMHot ||
		(s.State == contracts.FSMSleepPending && s.Pending.Abortable)):
		acts = append(acts, r.startPending(&s, contracts.SleepTriggerCritical,
			fmt.Sprintf("critical heat for %d polls", s.CritCount), now, in.Status)...)
	case s.BattCritCount >= batteryPolls && upgradable:
		acts = append(acts, r.startPending(&s, contracts.SleepTriggerBatteryCritical,
			fmt.Sprintf("battery %.0f°C >= %.0f°C", *batt, c.BatteryCriticalC), now, in.Status)...)
	case s.State == contracts.FSMHot && s.HotFor >= c.SeriousSleepAfter:
		acts = append(acts, r.startPending(&s, contracts.SleepTriggerSerious,
			fmt.Sprintf("hot for %s", humanize.Duration(s.HotFor)), now, in.Status)...)
	case s.BattHotCount >= batteryPolls && s.State != contracts.FSMSleepPending && !now.Before(s.CPUQuietUntil):
		acts = append(acts, r.startPending(&s, contracts.SleepTriggerBatteryTemp,
			fmt.Sprintf("battery %.0f°C >= %.0f°C", *batt, c.BatterySleepC), now, in.Status)...)
	case s.CPUHotCount >= cpuSleepPolls && s.State != contracts.FSMSleepPending && !now.Before(s.CPUQuietUntil):
		acts = append(acts, r.startPending(&s, contracts.SleepTriggerCPUTemp,
			fmt.Sprintf("CPU %.0f°C >= %.0f°C", *in.Reading.CPUC, *c.CPUSleepC), now, in.Status)...)
	}

	// Battery warning, unless a pending sleep already says more.
	if s.BattCalmCount >= c.CooledPolls {
		s.BattWarned = false
	}
	if s.BattWarmCount >= batteryPolls && !s.BattWarned && s.State != contracts.FSMSleepPending {
		s.BattWarned = true
		acts = append(acts, notifyAttention(contracts.PriorityHigh, fmt.Sprintf("Battery warm (%.0f°C) - open the bag", *batt),
			fmt.Sprintf("Sleeping at %.0f°C (abortable) or %.0f°C (not abortable). %s", c.BatterySleepC, c.BatteryCriticalC, in.Status),
			"battery", "warning"))
	}
	return s, acts
}

// batteryMissing notifies once when battery °C has been unreadable for
// thermalFailNotifyPolls polls, and again when it is back.
func (r thermalRules) batteryMissing(s *ThermalFSM, status string) []Action {
	switch {
	case s.BattMissCount >= thermalFailNotifyPolls && !s.BattMissNotified:
		s.BattMissNotified = true
		return []Action{notifyAttention(contracts.PriorityHigh, "Battery temperature unavailable",
			fmt.Sprintf("%d readings without battery °C. Battery heat protection is off; the thermal state rules still run. %s", s.BattMissCount, status),
			"warning")}
	case s.BattMissCount == 0 && s.BattMissNotified:
		s.BattMissNotified = false
		return []Action{notifyInfo(contracts.PriorityLow, "Battery temperature back", status, "white_check_mark")}
	}
	return nil
}

// Expire ends the grace period when it is over: SLEEP_PENDING becomes
// SLEEPING and ActSleepNow is returned. Otherwise s is returned unchanged.
func (r thermalRules) Expire(s ThermalFSM, now time.Time) (ThermalFSM, []Action) {
	s = s.clone()
	if s.State != contracts.FSMSleepPending || s.Pending == nil || now.Before(s.Pending.At) {
		return s, nil
	}
	p := s.Pending
	s.State, s.Pending = contracts.FSMSleeping, nil
	return s, []Action{{Kind: ActSleepNow, Trigger: p.Trigger, Msg: p.Reason}}
}

// Abort cancels a pending abortable sleep (stop --no-sleep, extend). by
// names the cause for the notification. With no pending sleep it returns
// s unchanged; with a critical one it returns contracts.ErrNotAbortable.
//
// After an abort the hot timer restarts, so a machine that stays hot sleeps
// again after another SeriousSleepAfter; the cpu_sleep_c trigger is quiet
// for the same period.
func (r thermalRules) Abort(s ThermalFSM, by string, now time.Time) (ThermalFSM, []Action, error) {
	s = s.clone()
	if s.State != contracts.FSMSleepPending || s.Pending == nil {
		return s, nil, nil
	}
	if !s.Pending.Abortable {
		return s, nil, contracts.ErrNotAbortable
	}
	p := s.Pending
	s.Pending = nil
	s.CPUQuietUntil = now.Add(r.Cfg.SeriousSleepAfter)
	s.CPUHotCount, s.BattHotCount = 0, 0
	s.BattWarned = true // the pending-sleep notice already said it
	if s.Last >= contracts.ThermalSerious {
		s.State = contracts.FSMHot
		s.HotSince, s.HotFor, s.LastHotNotify = now, 0, now
		if s.Alerted < s.Last {
			s.Alerted = s.Last
		}
	} else {
		s.State, s.FairNotified = contracts.FSMFair, true
		s.HotSince, s.HotFor, s.Alerted = time.Time{}, 0, contracts.ThermalUnknown
	}
	body := fmt.Sprintf("Cancelled by %s (was: %s). Still watching: a new sleep follows after %s of serious heat.",
		by, p.Reason, humanize.Duration(r.Cfg.SeriousSleepAfter))
	return s, []Action{
		{Kind: ActSleepCancel, Trigger: p.Trigger, Msg: by},
		notifyInfo(contracts.PriorityHigh, "Thermal sleep cancelled", body, "no_entry"),
	}, nil
}

func (r thermalRules) enterHot(s *ThermalFSM, st contracts.ThermalState, now time.Time, status string) Action {
	s.State = contracts.FSMHot
	s.HotSince, s.HotFor = now, 0
	s.FairNotified = false
	a, _ := r.hotNotify(s, st, now, status, true)
	return a
}

// hotNotify decides the HOT notification for a serious/critical poll:
// immediately on escalation, else every HotRenotify (when renotify).
func (r thermalRules) hotNotify(s *ThermalFSM, st contracts.ThermalState, now time.Time, status string, renotify bool) (Action, bool) {
	due := st > s.Alerted || (renotify && now.Sub(s.LastHotNotify) >= r.Cfg.HotRenotify)
	if !due {
		if st < s.Alerted {
			s.Alerted = st // critical -> serious: step down quietly
		}
		return Action{}, false
	}
	s.Alerted, s.LastHotNotify = st, now
	hot := humanize.Duration(now.Sub(s.HotSince))
	if st == contracts.ThermalCritical {
		return notifyAttention(contracts.PriorityUrgent, "Mac CRITICAL heat - take it out now",
			fmt.Sprintf("Critical for %d poll(s); sleeping after %d. Hot for %s. %s", s.CritCount, r.Cfg.CriticalPolls, hot, status),
			"rotating_light", "fire"), true
	}
	return notifyAttention(contracts.PriorityHigh, "Mac is HOT (serious) - open the bag",
		fmt.Sprintf("macOS is throttling. Hot for %s; sleeping after %s. %s", hot, humanize.Duration(r.Cfg.SeriousSleepAfter), status),
		"fire", "warning"), true
}

func (r thermalRules) startPending(s *ThermalFSM, trig contracts.SleepTrigger, reason string, now time.Time, status string) []Action {
	critical := trig == contracts.SleepTriggerCritical || trig == contracts.SleepTriggerBatteryCritical
	grace := r.Cfg.SeriousGrace
	if critical {
		grace = r.Cfg.CriticalGrace
	}
	at := now.Add(grace)
	if s.Pending != nil && s.Pending.At.Before(at) {
		at = s.Pending.At // an upgrade never postpones the sleep
	}
	s.State = contracts.FSMSleepPending
	s.Pending = &PendingSleep{Trigger: trig, Reason: reason, At: at, Abortable: !critical}
	in := humanize.Duration(at.Sub(now))
	var n Action
	if s.Pending.Abortable {
		n = notifyAttention(contracts.PriorityUrgent, fmt.Sprintf("Sleeping in %s: %s", in, reason),
			fmt.Sprintf("Sleeping at %s. Cancel: `bckp abort`. %s", humanize.ClockSec(at, now), status),
			"zzz", "fire")
	} else {
		n = notifyAttention(contracts.PriorityUrgent, fmt.Sprintf("Sleeping in %s: %s", in, reason),
			fmt.Sprintf("Sleeping at %s. Critical heat: this cannot be aborted. %s", humanize.ClockSec(at, now), status),
			"zzz", "rotating_light")
	}
	return []Action{{Kind: ActSleepPending, Trigger: trig, Msg: reason}, n}
}

func (s ThermalFSM) clone() ThermalFSM {
	if s.Pending != nil {
		p := *s.Pending
		s.Pending = &p
	}
	return s
}

func countIf(cond bool, n int) int {
	if cond {
		return n + 1
	}
	return 0
}
