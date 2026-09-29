package humanize

import (
	"testing"
	"time"
)

func TestDuration(t *testing.T) {
	for d, want := range map[time.Duration]string{
		-time.Second:                    "0s",
		0:                               "0s",
		45 * time.Second:                "45s",
		90 * time.Second:                "1m30s",
		9*time.Minute + 5*time.Second:   "9m05s",
		7 * time.Minute:                 "7m",
		12*time.Minute + 5*time.Second:  "12m",
		45 * time.Minute:                "45m",
		time.Hour:                       "1h",
		65 * time.Minute:                "1h05m",
		90 * time.Minute:                "1h30m",
		12 * time.Hour:                  "12h",
		225 * time.Minute:               "3h45m",
		7*time.Minute + 20*time.Second:  "7m20s",
		59*time.Minute + 40*time.Second: "1h",
		59600 * time.Millisecond:        "1m",
	} {
		if got := Duration(d); got != want {
			t.Errorf("Duration(%v) = %q, want %q", d, got, want)
		}
		if d >= time.Minute && d%time.Minute == 0 {
			if back, err := time.ParseDuration(Duration(d)); err != nil || back != d {
				t.Errorf("Duration(%v) = %q does not parse back (%v, %v)", d, Duration(d), back, err)
			}
		}
	}
}

func TestClock(t *testing.T) {
	now := time.Date(2026, 9, 29, 21, 10, 0, 0, time.UTC)
	if got := Clock(now.Add(time.Hour), now); got != "22:10" {
		t.Errorf("same day = %q", got)
	}
	if got := Clock(now.Add(12*time.Hour), now); got != "Wed 09:10" {
		t.Errorf("next day = %q", got)
	}
	if got := ClockSec(now.Add(15*time.Second), now); got != "21:10:15" {
		t.Errorf("with seconds = %q", got)
	}
	if OnOff(true) != "on" || OnOff(false) != "off" {
		t.Error("OnOff")
	}
}
