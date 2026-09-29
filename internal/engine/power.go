package engine

import (
	"fmt"
	"time"

	"github.com/ilias-t/bckp/internal/config"
	"github.com/ilias-t/bckp/internal/contracts"
)

// Power rails (docs/DESIGN.md "Power"):
//
//   - AC -> battery: notify immediately (p4, when NotifyOnBattery) and keep
//     running. Battery -> AC: notify at p2.
//   - On battery at or below BatteryFloor (0 disables): notify (p5) and end
//     the session without sleeping. Releasing the lease lets a closed lid
//     clamshell-sleep the Mac. This is checked on the first reading too.
//   - Read errors change nothing; after powerFailNotifyPolls in a row the
//     user is notified once, since the battery floor is then blind.

const powerFailNotifyPolls = 4

// powerRules is the power state machine's configuration.
type powerRules struct {
	Cfg config.Power
}

// PowerFSM is the power state machine's state. The zero value is the
// initial state.
type PowerFSM struct {
	// Known is true after the first good reading.
	Known   bool
	OnAC    bool
	Percent int
	// Ended is true once the floor ended the session.
	Ended        bool
	FailCount    int
	FailNotified bool
}

// PowerInput is one power poll.
type PowerInput struct {
	Reading contracts.PowerReading
	Err     error
	Status  string
}

// Step advances the machine by one poll. It is pure.
func (r powerRules) Step(s PowerFSM, in PowerInput, now time.Time) (PowerFSM, []Action) {
	if s.Ended {
		return s, nil
	}
	var acts []Action
	if in.Err != nil {
		s.FailCount++
		acts = append(acts, logf("power reading failed (%d in a row): %v", s.FailCount, in.Err))
		if s.FailCount >= powerFailNotifyPolls && !s.FailNotified {
			s.FailNotified = true
			acts = append(acts, notifyAttention(contracts.PriorityHigh, "Power readings failing",
				fmt.Sprintf("%d failed readings in a row (%v). The battery floor is blind; the daemon's 10%% backstop still runs.", s.FailCount, in.Err),
				"warning"))
		}
		return s, acts
	}
	s.FailCount, s.FailNotified = 0, false
	rd := in.Reading
	switch {
	case !s.Known:
		acts = append(acts, logf("power: %s", powerLabel(rd)))
	case s.OnAC && !rd.OnAC:
		acts = append(acts, logf("power: switched to battery at %d%%", rd.Percent))
		if r.Cfg.NotifyOnBattery {
			acts = append(acts, notifyAttention(contracts.PriorityHigh, fmt.Sprintf("On battery (%d%%)", rd.Percent),
				fmt.Sprintf("AC power lost; still running. Ends at %d%%. %s", r.Cfg.BatteryFloor, in.Status), "battery"))
		}
	case !s.OnAC && rd.OnAC:
		acts = append(acts, logf("power: back on AC at %d%%", rd.Percent))
		if r.Cfg.NotifyOnBattery {
			acts = append(acts, notifyInfo(contracts.PriorityLow, fmt.Sprintf("Back on AC (%d%%)", rd.Percent), in.Status, "electric_plug"))
		}
	}
	s.Known, s.OnAC, s.Percent = true, rd.OnAC, rd.Percent

	if r.Cfg.BatteryFloor > 0 && !rd.OnAC && rd.Percent <= r.Cfg.BatteryFloor {
		s.Ended = true
		acts = append(acts,
			notifyAttention(contracts.PriorityUrgent, fmt.Sprintf("Battery %d%% - backpack off", rd.Percent),
				fmt.Sprintf("On battery at or below the %d%% floor. Sleep guard released: a closed lid will sleep the Mac. %s", r.Cfg.BatteryFloor, in.Status),
				"battery", "warning"),
			Action{Kind: ActEndSession, EndReason: contracts.EndBatteryFloor,
				Msg: fmt.Sprintf("battery %d%% <= floor %d%%", rd.Percent, r.Cfg.BatteryFloor)})
	}
	return s, acts
}

func powerLabel(p contracts.PowerReading) string {
	src := "battery"
	if p.OnAC {
		src = "AC"
	}
	return fmt.Sprintf("%s %d%%", src, p.Percent)
}
