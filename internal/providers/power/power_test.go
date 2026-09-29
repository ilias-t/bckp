package power

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ilias-t/bckp/internal/contracts"
	"github.com/ilias-t/bckp/internal/fakes"
	"github.com/ilias-t/bckp/internal/paths"
)

// Fixtures: pmset_batt_ac_charged.txt and
// ioreg_clamshell_closed_sleepdisabled.txt were recorded on an Apple
// Silicon Mac (macOS 27.0) with the battery id and UUIDs anonymized and the ioreg dump
// trimmed to IOPMrootDomain. synth_* files are hand-edited variants for
// states that could not be recorded without changing the machine.

func fixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestParseBatt(t *testing.T) {
	tests := []struct {
		file string
		want Batt
	}{
		{"pmset_batt_ac_charged.txt", Batt{Source: "AC Power", OnAC: true, HasBattery: true, Percent: 100, State: "charged"}},
		{"synth_pmset_batt_discharging.txt", Batt{Source: "Battery Power", HasBattery: true, Percent: 85, State: "discharging"}},
		{"synth_pmset_batt_charging.txt", Batt{Source: "AC Power", OnAC: true, HasBattery: true, Percent: 42, State: "charging", Charging: true}},
		{"synth_pmset_batt_ac_not_charging.txt", Batt{Source: "AC Power", OnAC: true, HasBattery: true, Percent: 80, State: "AC attached"}},
		{"synth_pmset_batt_low_noestimate.txt", Batt{Source: "Battery Power", HasBattery: true, Percent: 7, State: "discharging"}},
		{"synth_pmset_batt_no_battery.txt", Batt{Source: "AC Power", OnAC: true}},
	}
	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) {
			got, err := ParseBatt(fixture(t, tt.file))
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("got %+v\nwant %+v", got, tt.want)
			}
		})
	}
	for _, bad := range []string{"", "garbage", "Now drawing from 'AC Power'\n -InternalBattery-0 (id=1)\t420%; charged;"} {
		if _, err := ParseBatt(bad); err == nil {
			t.Errorf("ParseBatt(%q) should fail", bad)
		}
	}
}

func TestParseClamshell(t *testing.T) {
	c, err := ParseClamshell(fixture(t, "ioreg_clamshell_closed_sleepdisabled.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if c != (Clamshell{LidClosed: true, SleepDisabled: true, CausesSleep: true}) {
		t.Errorf("closed = %+v", c)
	}
	c, err = ParseClamshell(fixture(t, "synth_ioreg_clamshell_open_sleepenabled.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if c != (Clamshell{CausesSleep: true}) {
		t.Errorf("open = %+v", c)
	}
	_, err = ParseClamshell(fixture(t, "synth_ioreg_clamshell_missing_sleepdisabled.txt"))
	if err == nil || !strings.Contains(err.Error(), "SleepDisabled") {
		t.Errorf("missing SleepDisabled: err = %v", err)
	}
	if _, err := ParseClamshell(""); err == nil {
		t.Error("empty output should fail")
	}
}

var (
	battCmd  = contracts.CommandLine(paths.BinPmset, BattArgs...)
	clamCmd  = contracts.CommandLine(paths.BinIoreg, ClamshellArgs...)
	fixedNow = time.Date(2026, 9, 28, 20, 0, 0, 0, time.UTC)
)

func newTest(ex contracts.Exec) *Source {
	s := New(ex).(*Source)
	s.now = func() time.Time { return fixedNow }
	return s
}

func TestRead(t *testing.T) {
	if battCmd != "/usr/bin/pmset -g batt" || clamCmd != "/usr/sbin/ioreg -r -k AppleClamshellState -d 4" {
		t.Fatalf("unexpected command lines %q %q", battCmd, clamCmd)
	}
	ex := fakes.NewExec().
		On(battCmd, fixture(t, "synth_pmset_batt_discharging.txt")).
		On(clamCmd, fixture(t, "ioreg_clamshell_closed_sleepdisabled.txt"))
	r, err := newTest(ex).Read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := contracts.PowerReading{OnAC: false, Percent: 85, LidClosed: true, SleepDisabled: true, At: fixedNow}
	if r != want {
		t.Errorf("got %+v\nwant %+v", r, want)
	}
}

func TestReadErrors(t *testing.T) {
	tests := []struct {
		name string
		ex   *fakes.Exec
		is   error
	}{
		{"pmset fails", fakes.NewExec().OnFail(battCmd, 1, "pmset: boom"), nil},
		{"no battery", fakes.NewExec().On(battCmd, fixture(t, "synth_pmset_batt_no_battery.txt")), contracts.ErrNoBattery},
		{"ioreg fails", fakes.NewExec().On(battCmd, fixture(t, "pmset_batt_ac_charged.txt")).OnFail(clamCmd, 1, ""), nil},
		{"ioreg missing key", fakes.NewExec().On(battCmd, fixture(t, "pmset_batt_ac_charged.txt")).
			On(clamCmd, fixture(t, "synth_ioreg_clamshell_missing_sleepdisabled.txt")), nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := newTest(tt.ex).Read(context.Background())
			if err == nil {
				t.Fatal("want error")
			}
			if tt.is != nil && !errors.Is(err, tt.is) {
				t.Errorf("err = %v, want %v", err, tt.is)
			}
		})
	}
}
