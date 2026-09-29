// Package humanize formats durations, clock times and switches the same way
// everywhere a person reads them: `status`, the TUI, notifications and
// `simulate`. Log fields keep Go's duration strings instead.
package humanize

import (
	"fmt"
	"time"
)

// Duration renders d for people: "45s", "1m05s", "7m", "2h", "1h05m".
// Under 10 minutes it keeps the seconds; longer durations are rounded to
// the minute. Negative durations render as "0s".
func Duration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	d = d.Round(time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d/time.Second))
	case d < 10*time.Minute && d%time.Minute != 0:
		return fmt.Sprintf("%dm%02ds", int(d/time.Minute), int(d%time.Minute/time.Second))
	}
	d = d.Round(time.Minute)
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	case d%time.Hour == 0:
		return fmt.Sprintf("%dh", int(d/time.Hour))
	}
	return fmt.Sprintf("%dh%02dm", int(d/time.Hour), int(d%time.Hour/time.Minute))
}

// Clock renders t as "15:04" when it falls on now's day, else "Mon 15:04",
// in now's location.
func Clock(t, now time.Time) string { return clock(t, now, "15:04") }

// ClockSec is Clock with seconds: "15:04:05" or "Mon 15:04:05".
func ClockSec(t, now time.Time) string { return clock(t, now, "15:04:05") }

func clock(t, now time.Time, layout string) string {
	t = t.In(now.Location())
	y1, m1, d1 := t.Date()
	y2, m2, d2 := now.Date()
	if y1 == y2 && m1 == m2 && d1 == d2 {
		return t.Format(layout)
	}
	return t.Format("Mon " + layout)
}

// OnOff renders a switch: "on" or "off".
func OnOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}
