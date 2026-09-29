package engine

import (
	"errors"
	"strings"
	"testing"

	"github.com/ilias-t/bckp/internal/config"
	"github.com/ilias-t/bckp/internal/contracts"
)

func TestPowerStep(t *testing.T) {
	ac := func(pct int) PowerInput { return PowerInput{Reading: contracts.PowerReading{OnAC: true, Percent: pct}} }
	bat := func(pct int) PowerInput {
		return PowerInput{Reading: contracts.PowerReading{OnAC: false, Percent: pct}}
	}
	fail := PowerInput{Err: errors.New("pmset failed")}

	tests := []struct {
		name  string
		cfg   func(*config.Power)
		steps []PowerInput
		want  []string
		ended bool
	}{
		{name: "AC stays quiet", steps: []PowerInput{ac(100), ac(100), ac(99)}},
		{
			name: "AC to battery notifies and keeps running", steps: []PowerInput{ac(90), bat(89), bat(60)},
			want: []string{"2:p4:On battery (89%)"},
		},
		{
			name: "back on AC", steps: []PowerInput{ac(90), bat(80), ac(80)},
			want: []string{"2:p4:On battery (80%)", "3:p2:Back on AC (80%)"},
		},
		{
			name: "battery floor ends the session", steps: []PowerInput{ac(90), bat(30), bat(26), bat(25), bat(20)},
			want:  []string{"2:p4:On battery (30%)", "4:p5:Battery 25% - backpack off", "4:end-session:battery_floor"},
			ended: true,
		},
		{
			name: "floor applies to the first reading", steps: []PowerInput{bat(10)},
			want: []string{"1:p5:Battery 10% - backpack off", "1:end-session:battery_floor"}, ended: true,
		},
		{name: "low battery on AC is fine", steps: []PowerInput{ac(5), ac(3)}},
		{
			name: "floor 0 disables", cfg: func(c *config.Power) { c.BatteryFloor = 0 },
			steps: []PowerInput{ac(50), bat(3)}, want: []string{"2:p4:On battery (3%)"},
		},
		{
			name: "notify_on_battery=false keeps the floor", cfg: func(c *config.Power) { c.NotifyOnBattery = false },
			steps: []PowerInput{ac(50), bat(40), bat(20)},
			want:  []string{"3:p5:Battery 20% - backpack off", "3:end-session:battery_floor"}, ended: true,
		},
		{
			name: "read errors change nothing and notify once after 4", steps: []PowerInput{ac(90), fail, fail, fail, fail, fail, ac(90)},
			want: []string{"5:p4:Power readings failing"},
		},
		{
			name: "error between AC and battery still detects the switch", steps: []PowerInput{ac(90), fail, bat(80)},
			want: []string{"3:p4:On battery (80%)"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := powerRules{Cfg: config.Default().Power}
			if tc.cfg != nil {
				tc.cfg(&r.Cfg)
			}
			var s PowerFSM
			var got []string
			now := t0
			for i, in := range tc.steps {
				var acts []Action
				s, acts = r.Step(s, in, now)
				for _, a := range acts {
					if d := describe(i+1, a); d != "" {
						got = append(got, d)
					}
				}
				now = now.Add(poll)
			}
			if strings.Join(got, "\n") != strings.Join(tc.want, "\n") {
				t.Errorf("actions:\n got %q\nwant %q", got, tc.want)
			}
			if s.Ended != tc.ended {
				t.Errorf("ended = %v, want %v", s.Ended, tc.ended)
			}
		})
	}
}
