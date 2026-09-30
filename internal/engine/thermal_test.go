package engine

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ilias-t/bckp/internal/config"
	"github.com/ilias-t/bckp/internal/contracts"
)

var t0 = time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)

const poll = 15 * time.Second

// thermalSeq expands "n x4 f s x40 c e u 96C 41B" into readings: n/f/s/c
// are nominal/fair/serious/critical, u is ThermalUnknown, e is a read
// error, "xN" repeats the previous token to N total, "<n>C" sets CPU °C
// and "<n>B" battery °C for the following readings ("-B": unreadable).
// Battery °C starts at a calm 30.
func thermalSeq(t *testing.T, spec string) []ThermalInput {
	t.Helper()
	var out []ThermalInput
	var cpu *float64
	batt30 := 30.0
	batt := &batt30
	names := map[string]contracts.ThermalState{
		"n": contracts.ThermalNominal, "f": contracts.ThermalFair, "s": contracts.ThermalSerious,
		"c": contracts.ThermalCritical, "u": contracts.ThermalUnknown,
	}
	for _, tok := range strings.Fields(spec) {
		switch {
		case strings.HasPrefix(tok, "x"):
			n, err := strconv.Atoi(tok[1:])
			if err != nil || len(out) == 0 {
				t.Fatalf("bad repeat %q", tok)
			}
			for i := 1; i < n; i++ {
				out = append(out, out[len(out)-1])
			}
		case tok == "-B":
			batt = nil
		case strings.HasSuffix(tok, "B"):
			v, err := strconv.ParseFloat(strings.TrimSuffix(tok, "B"), 64)
			if err != nil {
				t.Fatalf("bad battery temp %q", tok)
			}
			batt = &v
		case strings.HasSuffix(tok, "C"):
			v, err := strconv.ParseFloat(strings.TrimSuffix(tok, "C"), 64)
			if err != nil {
				t.Fatalf("bad temp %q", tok)
			}
			cpu = &v
		case tok == "e":
			out = append(out, ThermalInput{Err: errors.New("shim failed")})
		default:
			st, ok := names[tok]
			if !ok {
				t.Fatalf("bad token %q", tok)
			}
			out = append(out, ThermalInput{Reading: contracts.ThermalReading{State: st, CPUC: cpu, BatteryC: batt}})
		}
	}
	return out
}

// act is a compact action description: "poll#:kind:detail".
func describe(poll int, a Action) string {
	switch a.Kind {
	case ActNotify:
		return fmt.Sprintf("%d:p%d:%s", poll, a.Notification.Priority, a.Notification.Title)
	case ActSleepPending, ActSleepCancel, ActSleepNow:
		return fmt.Sprintf("%d:%s:%s", poll, a.Kind, a.Trigger)
	case ActEndSession:
		return fmt.Sprintf("%d:%s:%s", poll, a.Kind, a.EndReason)
	case ActLog:
		return ""
	default:
		return fmt.Sprintf("%d:%s", poll, a.Kind)
	}
}

func runThermal(t *testing.T, r thermalRules, s ThermalFSM, spec string, start time.Time) (ThermalFSM, []string, time.Time) {
	t.Helper()
	var got []string
	now := start
	for i, in := range thermalSeq(t, spec) {
		var acts []Action
		s, acts = r.Step(s, in, now)
		for _, a := range acts {
			if d := describe(i+1, a); d != "" {
				got = append(got, d)
			}
		}
		now = now.Add(poll)
	}
	return s, got, now
}

func defaultThermal() thermalRules {
	cfg := config.Default().Thermal
	legacyThermalTiming(&cfg)
	return thermalRules{Cfg: cfg, Poll: poll}
}

func TestThermalStep(t *testing.T) {
	cpu95 := 95.0
	tests := []struct {
		name  string
		cfg   func(*config.Thermal)
		spec  string
		state contracts.FSMState
		want  []string
	}{
		{name: "nominal stays quiet", spec: "n x10", state: contracts.FSMNominal},
		{name: "one fair poll is not FAIR", spec: "n f n f n", state: contracts.FSMNominal},
		{
			name: "fair x2 enters FAIR and notifies", spec: "n f x2", state: contracts.FSMFair,
			want: []string{"3:p3:Mac warming up (fair)"},
		},
		{
			name: "back to nominal after 3 nominal polls", spec: "f x2 n x2 f n x3", state: contracts.FSMNominal,
			want: []string{"2:p3:Mac warming up (fair)", "8:p2:Mac back to nominal"},
		},
		{
			name: "fair notification rate-limited, and no back-to-nominal for a silent episode",
			spec: "f x2 n x3 f x2 n x3", state: contracts.FSMNominal,
			want: []string{"2:p3:Mac warming up (fair)", "5:p2:Mac back to nominal"},
		},
		{
			name: "serious enters HOT immediately", spec: "n s", state: contracts.FSMHot,
			want: []string{"2:p4:Mac is HOT (serious) - open the bag"},
		},
		{
			name: "HOT re-notifies every 10m",
			cfg:  func(c *config.Thermal) { c.SeriousSleepAfter = time.Hour },
			// HOT at poll 1; 10m = 40 polls later is poll 41; 20m is poll 81.
			spec: "s x81", state: contracts.FSMHot,
			want: []string{
				"1:p4:Mac is HOT (serious) - open the bag",
				"41:p4:Mac is HOT (serious) - open the bag",
				"81:p4:Mac is HOT (serious) - open the bag",
			},
		},
		{
			name: "HOT for 10 minutes starts an abortable 60s grace", spec: "n x4 f x3 s x41", state: contracts.FSMSleepPending,
			want: []string{
				"6:p3:Mac warming up (fair)",
				"8:p4:Mac is HOT (serious) - open the bag",
				"48:sleep-pending:serious",
				"48:p5:Sleeping in 1m: hot for 10m",
			},
		},
		{
			name: "one calm poll does not reset the hot timer", spec: "s x20 f s x21", state: contracts.FSMSleepPending,
			// The calm poll's interval is not hot time, so the sleep comes
			// one poll after the 10m re-notification.
			want: []string{
				"1:p4:Mac is HOT (serious) - open the bag", "41:p4:Mac is HOT (serious) - open the bag",
				"42:sleep-pending:serious", "42:p5:Sleeping in 1m: hot for 10m",
			},
		},
		{
			name: "two calm polls cool down, then nominal x3", spec: "s x3 f x2 n x3", state: contracts.FSMNominal,
			want: []string{
				"1:p4:Mac is HOT (serious) - open the bag",
				"5:p3:Mac cooled down (fair)",
				"8:p2:Mac back to nominal",
			},
		},
		{
			name: "cooled down resets the hot timer", spec: "s x30 n x2 s x39", state: contracts.FSMHot,
			want: []string{"1:p4:Mac is HOT (serious) - open the bag", "32:p3:Mac cooled down (nominal)", "33:p4:Mac is HOT (serious) - open the bag"},
		},
		{
			name: "critical x2 starts a 15s non-abortable grace", spec: "n c x2", state: contracts.FSMSleepPending,
			want: []string{"2:p5:Mac CRITICAL heat - take it out now", "3:sleep-pending:critical", "3:p5:Sleeping in 15s: critical heat for 2 polls"},
		},
		{
			name: "serious then critical escalates immediately", spec: "s x3 c s", state: contracts.FSMHot,
			want: []string{"1:p4:Mac is HOT (serious) - open the bag", "4:p5:Mac CRITICAL heat - take it out now"},
		},
		{
			name: "non-consecutive criticals do not trigger", spec: "c s c s", state: contracts.FSMHot,
			want: []string{"1:p5:Mac CRITICAL heat - take it out now", "3:p5:Mac CRITICAL heat - take it out now"},
		},
		{
			name: "critical during a serious grace upgrades it", spec: "s x41 c x2", state: contracts.FSMSleepPending,
			want: []string{
				"1:p4:Mac is HOT (serious) - open the bag",
				"41:sleep-pending:serious", "41:p5:Sleeping in 1m: hot for 10m",
				"42:p5:Mac CRITICAL heat - take it out now",
				"43:sleep-pending:critical", "43:p5:Sleeping in 15s: critical heat for 2 polls",
			},
		},
		{
			name: "failed readings change nothing and notify once after 4",
			spec: "s e x6 s", state: contracts.FSMHot,
			want: []string{"1:p4:Mac is HOT (serious) - open the bag", "5:p4:Thermal readings failing", "8:p2:Thermal readings back"},
		},
		{
			name: "unknown counts as a failed reading, not as nominal", spec: "f x2 u x3 n x2", state: contracts.FSMFair,
			want: []string{"2:p3:Mac warming up (fair)"},
		},
		{
			name: "failures do not advance the hot timer",
			// 1 + 40 serious polls would trigger; 4 failures in between
			// plus the uncounted interval after them delay it by one poll.
			spec: "s x20 e x4 s x21", state: contracts.FSMHot,
			want: []string{
				"1:p4:Mac is HOT (serious) - open the bag", "24:p4:Thermal readings failing",
				"25:p2:Thermal readings back", "41:p4:Mac is HOT (serious) - open the bag",
			},
		},
		{
			name: "cpu_sleep_c triggers an abortable sleep after 2 polls",
			cfg:  func(c *config.Thermal) { c.CPUSleepC = &cpu95 },
			spec: "90C n x3 96C n x2", state: contracts.FSMSleepPending,
			want: []string{"5:sleep-pending:cpu_temp", "5:p5:Sleeping in 1m: CPU 96°C >= 95°C"},
		},
		{
			name: "cpu_sleep_c unset ignores temperature", spec: "120C n x5", state: contracts.FSMNominal,
		},
		{name: "one warm battery poll does not warn", spec: "41B n 30B n 41B n 30B n", state: contracts.FSMNominal},
		{
			name: "warm battery x2 warns once", spec: "41B n x5", state: contracts.FSMNominal,
			want: []string{"2:p4:Battery warm (41°C) - open the bag"},
		},
		{
			name: "battery warning re-arms after cooled_polls below warn", spec: "41B n x2 30B n x2 41B n x2", state: contracts.FSMNominal,
			want: []string{"2:p4:Battery warm (41°C) - open the bag", "6:p4:Battery warm (41°C) - open the bag"},
		},
		{
			name: "battery_sleep_c x2 starts an abortable sleep instead of the warning", spec: "46B n x2", state: contracts.FSMSleepPending,
			want: []string{"2:sleep-pending:battery_temp", "2:p5:Sleeping in 1m: battery 46°C >= 45°C"},
		},
		{
			name: "battery sleep fires while the thermal state stays nominal", spec: "41B n x2 46B n x2", state: contracts.FSMSleepPending,
			want: []string{"2:p4:Battery warm (41°C) - open the bag", "4:sleep-pending:battery_temp", "4:p5:Sleeping in 1m: battery 46°C >= 45°C"},
		},
		{
			name: "battery_critical_c x2 starts a non-abortable sleep", spec: "51B n x2", state: contracts.FSMSleepPending,
			want: []string{"2:sleep-pending:battery_critical", "2:p5:Sleeping in 15s: battery 51°C >= 50°C"},
		},
		{
			name: "critical battery upgrades a pending battery sleep", spec: "46B n x2 51B n x2", state: contracts.FSMSleepPending,
			want: []string{
				"2:sleep-pending:battery_temp", "2:p5:Sleeping in 1m: battery 46°C >= 45°C",
				"4:sleep-pending:battery_critical", "4:p5:Sleeping in 15s: battery 51°C >= 50°C",
			},
		},
		{
			name: "unreadable battery notifies once after 4 polls, and when back", spec: "-B n x6 30B n", state: contracts.FSMNominal,
			want: []string{"4:p4:Battery temperature unavailable", "7:p2:Battery temperature back"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := defaultThermal()
			if tc.cfg != nil {
				tc.cfg(&r.Cfg)
			}
			s, got, _ := runThermal(t, r, NewThermalFSM(), tc.spec, t0)
			if s.State != tc.state {
				t.Errorf("state = %s, want %s", s.State, tc.state)
			}
			if strings.Join(got, "\n") != strings.Join(tc.want, "\n") {
				t.Errorf("actions:\n got %q\nwant %q", got, tc.want)
			}
		})
	}
}

func TestThermalHotTimerPausesOnFailures(t *testing.T) {
	r := defaultThermal()
	s, _, _ := runThermal(t, r, NewThermalFSM(), "s x20 e x4 s x22", t0)
	if s.State != contracts.FSMSleepPending {
		t.Fatalf("state = %s, want SLEEP_PENDING one poll later than without failures", s.State)
	}
	if s.HotFor != 10*time.Minute {
		t.Fatalf("HotFor = %s", s.HotFor)
	}
}

func TestThermalExpire(t *testing.T) {
	r := defaultThermal()
	s, _, now := runThermal(t, r, NewThermalFSM(), "s x41", t0)
	pendAt := now.Add(-poll)
	if s.Pending == nil || !s.Pending.Abortable || !s.Pending.At.Equal(pendAt.Add(60*time.Second)) {
		t.Fatalf("pending = %+v", s.Pending)
	}
	if s2, acts := r.Expire(s, pendAt.Add(59*time.Second)); s2.State != contracts.FSMSleepPending || len(acts) != 0 {
		t.Fatalf("expired early: %s %v", s2.State, acts)
	}
	s2, acts := r.Expire(s, pendAt.Add(60*time.Second))
	if s2.State != contracts.FSMSleeping || s2.Pending != nil || len(acts) != 1 || acts[0].Kind != ActSleepNow || acts[0].Trigger != contracts.SleepTriggerSerious {
		t.Fatalf("expire = %s %+v", s2.State, acts)
	}
	if s.Pending == nil {
		t.Fatal("Expire mutated its input")
	}
	// Step also expires, and SLEEPING absorbs further readings.
	s3, acts := r.Step(s, ThermalInput{Reading: contracts.ThermalReading{State: contracts.ThermalNominal}}, pendAt.Add(61*time.Second))
	if s3.State != contracts.FSMSleeping || len(acts) != 1 || acts[0].Kind != ActSleepNow {
		t.Fatalf("step expire = %s %+v", s3.State, acts)
	}
	if s4, acts := r.Step(s3, ThermalInput{Reading: contracts.ThermalReading{State: contracts.ThermalCritical}}, pendAt.Add(90*time.Second)); s4.State != contracts.FSMSleeping || len(acts) != 0 {
		t.Fatalf("SLEEPING must absorb: %s %+v", s4.State, acts)
	}
}

func TestThermalUpgradeNeverPostpones(t *testing.T) {
	r := defaultThermal()
	r.Cfg.CriticalGrace = 45 * time.Second
	s, _, now := runThermal(t, r, NewThermalFSM(), "s x41 n", t0) // pending at poll 41, grace ends 60s later
	at := s.Pending.At
	s, _, _ = runThermal(t, r, s, "c c", now) // upgrade at poll 44, 15s before at: now+45s would be later
	if s.Pending.Trigger != contracts.SleepTriggerCritical || s.Pending.Abortable || !s.Pending.At.Equal(at) {
		t.Fatalf("pending = %+v, want critical at %v", s.Pending, at)
	}
}

func TestThermalAbort(t *testing.T) {
	r := defaultThermal()
	s, _, now := runThermal(t, r, NewThermalFSM(), "s x41", t0)
	s2, acts, err := r.Abort(s, "extend", now)
	if err != nil || s2.State != contracts.FSMHot || s2.Pending != nil || s2.HotFor != 0 {
		t.Fatalf("abort = %s %+v %v", s2.State, s2.Pending, err)
	}
	if len(acts) != 2 || acts[0].Kind != ActSleepCancel || acts[1].Notification.Title != "Thermal sleep cancelled" {
		t.Fatalf("abort actions = %+v", acts)
	}
	// Still serious: the next sleep needs another full 10 minutes from
	// the abort, i.e. the 40th poll after it.
	s3, got, _ := runThermal(t, r, s2, "s x39", now.Add(poll))
	if s3.State != contracts.FSMHot {
		t.Fatalf("re-triggered too early: %v", got)
	}
	s3, _, _ = runThermal(t, r, s3, "s", now.Add(40*poll))
	if s3.State != contracts.FSMSleepPending {
		t.Fatalf("state after another 10m = %s", s3.State)
	}

	// Cooled during the grace: abort lands in FAIR, owing a nominal notice.
	s, _, now = runThermal(t, r, NewThermalFSM(), "s x41 n", t0)
	s2, _, err = r.Abort(s, "stop --no-sleep", now)
	if err != nil || s2.State != contracts.FSMFair || !s2.FairNotified {
		t.Fatalf("abort while cool = %s %v", s2.State, err)
	}

	// Critical: not abortable, unchanged.
	s, _, now = runThermal(t, r, NewThermalFSM(), "c x2", t0)
	s2, acts, err = r.Abort(s, "extend", now)
	if !errors.Is(err, contracts.ErrNotAbortable) || s2.State != contracts.FSMSleepPending || s2.Pending == nil || len(acts) != 0 {
		t.Fatalf("critical abort = %s %v %v", s2.State, acts, err)
	}

	// Nothing pending: no-op.
	s2, acts, err = r.Abort(NewThermalFSM(), "extend", now)
	if err != nil || s2.State != contracts.FSMNominal || len(acts) != 0 {
		t.Fatalf("idle abort = %s %v %v", s2.State, acts, err)
	}
}

func TestThermalCPUTriggerQuietAfterAbort(t *testing.T) {
	cpu95 := 95.0
	r := defaultThermal()
	r.Cfg.CPUSleepC = &cpu95
	s, _, now := runThermal(t, r, NewThermalFSM(), "96C n x2", t0)
	if s.State != contracts.FSMSleepPending {
		t.Fatalf("state = %s", s.State)
	}
	s, _, err := r.Abort(s, "extend", now)
	if err != nil {
		t.Fatal(err)
	}
	s, _, _ = runThermal(t, r, s, "96C n x39", now) // < 10m
	if s.State == contracts.FSMSleepPending {
		t.Fatal("cpu trigger fired during the quiet period")
	}
	s, _, _ = runThermal(t, r, s, "96C n x2", now.Add(40*poll))
	if s.State != contracts.FSMSleepPending {
		t.Fatalf("cpu trigger after quiet period: %s", s.State)
	}
}

func TestThermalDefaultTimings(t *testing.T) {
	r := thermalRules{Cfg: config.Default().Thermal, Poll: poll}
	// 3m hot is 12 polls after entering HOT; a 30s grace.
	s, got, _ := runThermal(t, r, NewThermalFSM(), "s x13", t0)
	want := []string{"1:p4:Mac is HOT (serious) - open the bag", "13:sleep-pending:serious", "13:p5:Sleeping in 30s: hot for 3m"}
	if s.State != contracts.FSMSleepPending || strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("state %s, actions:\n got %q\nwant %q", s.State, got, want)
	}
	// Under 2 minutes of calm (8 polls) does not reset the hot timer, so
	// swinging between serious and fair still ends in a sleep.
	s, _, _ = runThermal(t, r, NewThermalFSM(), "s x6 f x7 s x7", t0)
	if s.State != contracts.FSMSleepPending {
		t.Fatalf("state = %s, want SLEEP_PENDING despite brief cool spells", s.State)
	}
}

func TestThermalBatteryTriggerQuietAfterAbort(t *testing.T) {
	r := defaultThermal()
	s, _, now := runThermal(t, r, NewThermalFSM(), "46B n x2", t0)
	if s.State != contracts.FSMSleepPending || !s.Pending.Abortable {
		t.Fatalf("state = %s %+v", s.State, s.Pending)
	}
	s, _, err := r.Abort(s, "extend", now)
	if err != nil {
		t.Fatal(err)
	}
	s, _, _ = runThermal(t, r, s, "46B n x39", now) // < 10m
	if s.State == contracts.FSMSleepPending {
		t.Fatal("battery trigger fired during the quiet period")
	}
	s, _, _ = runThermal(t, r, s, "46B n x2", now.Add(40*poll))
	if s.State != contracts.FSMSleepPending {
		t.Fatalf("battery trigger after quiet period: %s", s.State)
	}

	// The quiet period never mutes the critical battery threshold.
	s, _, now = runThermal(t, r, NewThermalFSM(), "46B n x2", t0)
	s, _, _ = r.Abort(s, "extend", now)
	s, _, _ = runThermal(t, r, s, "51B n x2", now)
	if s.State != contracts.FSMSleepPending || s.Pending.Abortable || s.Pending.Trigger != contracts.SleepTriggerBatteryCritical {
		t.Fatalf("critical battery during quiet period: %s %+v", s.State, s.Pending)
	}
}
