package thermal

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ilias-t/bckp/internal/contracts"
	"github.com/ilias-t/bckp/internal/fakes"
	"github.com/ilias-t/bckp/internal/providers/macmon"
)

// TestStateOnThisMachine calls the real cgo shim.
func TestStateOnThisMachine(t *testing.T) {
	s, err := State()
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if s < contracts.ThermalNominal || s > contracts.ThermalCritical {
		t.Fatalf("State = %v, want nominal..critical", s)
	}
	t.Logf("thermal state: %v", s)
}

// TestBatteryOnThisMachine calls the real cgo shim. Machines without a
// battery (CI VMs, desktops) read nil.
func TestBatteryOnThisMachine(t *testing.T) {
	c := BatteryC()
	if c == nil {
		t.Skip("no battery temperature on this machine")
	}
	if *c < 1 || *c > 99 {
		t.Fatalf("BatteryC = %v, want a plausible cell temperature", *c)
	}
	t.Logf("battery: %.1f°C", *c)
}

func TestHottestPlausible(t *testing.T) {
	tests := []struct {
		name string
		in   []float64
		want float64 // 0: nil
	}{
		{"none", nil, 0},
		{"hottest of several gauges", []float64{33.0, 34.8, 33.7}, 34.8},
		{"implausible values dropped", []float64{-9202.8, 0, 33.5, 150}, 33.5},
		{"only implausible", []float64{-9202.8, 0, 120}, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := hottestPlausible(tc.in)
			switch {
			case tc.want == 0 && got != nil:
				t.Errorf("got %v, want nil", *got)
			case tc.want != 0 && (got == nil || *got != tc.want):
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestFromNS(t *testing.T) {
	want := map[int]contracts.ThermalState{
		0: contracts.ThermalNominal,
		1: contracts.ThermalFair,
		2: contracts.ThermalSerious,
		3: contracts.ThermalCritical,
	}
	for ns, w := range want {
		if got, err := fromNS(ns); err != nil || got != w {
			t.Errorf("fromNS(%d) = %v, %v; want %v", ns, got, err, w)
		}
	}
	for _, bad := range []int{-1, 4, 99} {
		if got, err := fromNS(bad); err == nil || got != contracts.ThermalUnknown {
			t.Errorf("fromNS(%d) = %v, %v; want unknown + error", bad, got, err)
		}
	}
}

const macmonBin = "/opt/homebrew/bin/macmon"

var macmonCmd = contracts.CommandLine(macmonBin, macmon.Args...)

var at = time.Date(2026, 9, 28, 20, 0, 0, 0, time.UTC)

func newTest(ex contracts.Exec, opts Options, st contracts.ThermalState, stErr error) *Source {
	s := New(ex, opts).(*Source)
	s.state = func() (contracts.ThermalState, error) { return st, stErr }
	s.battery = func() *float64 { b := 36.5; return &b }
	s.now = func() time.Time { return at }
	return s
}

func macmonFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "macmon", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func skipIfRoot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("macmon refuses to run as root")
	}
}

func TestReadCombines(t *testing.T) {
	skipIfRoot(t)
	ex := fakes.NewExec().On(macmonCmd, macmonFixture(t, "synth_macmon_pipe_gpu_idle.json"))
	r, err := newTest(ex, Options{MacmonPath: macmonBin}, contracts.ThermalSerious, nil).Read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if r.State != contracts.ThermalSerious || !r.At.Equal(at) {
		t.Errorf("reading = %+v", r)
	}
	if r.CPUC == nil || *r.CPUC < 41 || *r.CPUC > 43 {
		t.Errorf("CPUC = %v", r.CPUC)
	}
	if r.GPUC != nil {
		t.Errorf("idle GPU %v°C should be discarded", *r.GPUC)
	}
}

func TestReadMacmonFailureIsNotAnError(t *testing.T) {
	skipIfRoot(t)
	ex := fakes.NewExec().On(macmonCmd, macmonFixture(t, "synth_macmon_garbage.txt"))
	r, err := newTest(ex, Options{MacmonPath: macmonBin}, contracts.ThermalNominal, nil).Read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if r.State != contracts.ThermalNominal || r.CPUC != nil || r.GPUC != nil {
		t.Errorf("reading = %+v", r)
	}
}

func TestReadDisableMacmon(t *testing.T) {
	ex := fakes.NewExec()
	r, err := newTest(ex, Options{DisableMacmon: true}, contracts.ThermalFair, nil).Read(context.Background())
	if err != nil || r.State != contracts.ThermalFair || r.CPUC != nil {
		t.Fatalf("reading = %+v, %v", r, err)
	}
	// Battery °C is in-process, so the root daemon gets it too.
	if r.BatteryC == nil || *r.BatteryC != 36.5 {
		t.Errorf("BatteryC = %v, want 36.5", r.BatteryC)
	}
	if n := len(ex.Calls()); n != 0 {
		t.Errorf("ran %d commands with macmon disabled", n)
	}
}

func TestReadStateError(t *testing.T) {
	ex := fakes.NewExec()
	boom := errors.New("boom")
	r, err := newTest(ex, Options{MacmonPath: macmonBin}, contracts.ThermalSerious, boom).Read(context.Background())
	if !errors.Is(err, boom) || r.State != contracts.ThermalUnknown {
		t.Fatalf("reading = %+v, %v", r, err)
	}
	if n := len(ex.Calls()); n != 0 {
		t.Errorf("ran macmon after a state failure")
	}
}
