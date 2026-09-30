package contracts

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestThermalStateNamesAndOrder(t *testing.T) {
	if !(ThermalUnknown < ThermalNominal && ThermalNominal < ThermalFair &&
		ThermalFair < ThermalSerious && ThermalSerious < ThermalCritical) {
		t.Fatal("thermal states out of order")
	}
	for _, s := range []ThermalState{ThermalUnknown, ThermalNominal, ThermalFair, ThermalSerious, ThermalCritical} {
		got, err := ParseThermalState(strings.ToUpper(s.String()))
		if err != nil || got != s {
			t.Errorf("parse %q = %v, %v", s, got, err)
		}
	}
	if _, err := ParseThermalState("toasty"); err == nil {
		t.Error("expected error for unknown name")
	}
	if ThermalState(42).String() != "ThermalState(42)" {
		t.Errorf("out-of-range String = %q", ThermalState(42).String())
	}
	var zero ThermalReading
	if zero.State != ThermalUnknown {
		t.Error("zero reading must be unknown")
	}
}

func TestThermalReadingJSON(t *testing.T) {
	c := 71.5
	r := ThermalReading{State: ThermalSerious, CPUC: &c, At: time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)}
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"state":"serious","cpu_c":71.5,"at":"2026-09-28T12:00:00Z"}`
	if string(b) != want {
		t.Fatalf("got %s\nwant %s", b, want)
	}
	var back ThermalReading
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back, r) {
		t.Fatalf("round trip: %+v", back)
	}
}

func TestSnapshotJSONRoundTrip(t *testing.T) {
	at := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	s := Snapshot{
		At:  at,
		PID: 123,
		Session: SessionStatus{
			StartedAt: at.Add(-time.Hour), Forever: true, CapAt: at.Add(11 * time.Hour), RemainingS: 39600,
		},
		Thermal: ThermalStatus{FSM: FSMHot, Reading: &ThermalReading{State: ThermalSerious, At: at}, HotSince: at.Add(-2 * time.Minute)},
		Power:   &PowerReading{OnAC: false, Percent: 80, LidClosed: true, SleepDisabled: true, At: at},
		Network: NetworkStatus{Mode: NetworkWatchdog, Online: true, Latest: &NetSnapshot{Online: true, Iface: "en10", At: at}},
		Lease:   LeaseStatus{Held: true, ID: "abc", Expires: at.Add(90 * time.Second)},
		SleepPending: &SleepPending{
			Trigger: SleepTriggerSerious, Reason: "hot for 10m", At: at.Add(time.Minute), RemainingS: 60, Abortable: true,
		},
		Notifications: []NotificationRecord{{
			Notification: Notification{Priority: 4, Title: "Hot", Body: "serious", Tags: []string{"fire"}},
			At:           at, Status: NotificationSent,
		}},
		Errors: map[string]string{"network": "probe failed"},
	}
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	// Zero optional timestamps are omitted, not sent as year 1.
	if strings.Contains(string(b), "0001-01-01") {
		t.Errorf("zero time leaked: %s", b)
	}
	if strings.Contains(string(b), `"ends_at"`) {
		t.Errorf("forever session must omit ends_at: %s", b)
	}
	var back Snapshot
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back, s) {
		t.Fatalf("round trip mismatch:\n got %+v\nwant %+v", back, s)
	}
}

func TestNotificationValidate(t *testing.T) {
	if err := (Notification{Title: "x"}).Validate(); err != nil {
		t.Errorf("zero priority should be allowed: %v", err)
	}
	if err := (Notification{Priority: 6, Title: "x"}).Validate(); err == nil {
		t.Error("priority 6 should fail")
	}
	if err := (Notification{Priority: 3}).Validate(); err == nil {
		t.Error("empty notification should fail")
	}
}

func TestCommandLineAndExitError(t *testing.T) {
	if got := CommandLine("/usr/bin/pmset", "-g", "batt"); got != "/usr/bin/pmset -g batt" {
		t.Errorf("CommandLine = %q", got)
	}
	if got := CommandLine("/usr/bin/true"); got != "/usr/bin/true" {
		t.Errorf("CommandLine = %q", got)
	}
	e := &ExitError{Cmd: "x", ExitCode: 2, Stderr: []byte("bad\n")}
	if e.Error() != "x: exit status 2: bad" {
		t.Errorf("Error = %q", e.Error())
	}
}

func TestNetSnapshotLink(t *testing.T) {
	for _, tc := range []struct {
		name string
		snap NetSnapshot
		want NetLink
	}{
		{"no route", NetSnapshot{Iface: "en10", WiFiIface: "en0"}, LinkNone},
		{"iPhone USB", NetSnapshot{DefaultIface: "en10", DefaultGateway: HotspotGateway, Iface: "en10", WiFiIface: "en0"}, LinkHotspotUSB},
		{"hotspot over Wi-Fi", NetSnapshot{DefaultIface: "en0", DefaultGateway: HotspotGateway, Iface: "en10", WiFiIface: "en0"}, LinkHotspotWiFi},
		{"other Wi-Fi", NetSnapshot{DefaultIface: "en0", DefaultGateway: "192.0.2.1", Iface: "en10", WiFiIface: "en0"}, LinkWiFi},
		{"Ethernet", NetSnapshot{DefaultIface: "en7", DefaultGateway: "198.51.100.1", Iface: "en10", WiFiIface: "en0"}, LinkOther},
		// Without the Wi-Fi interface the hotspot gateway alone proves
		// nothing (the USB link uses it too).
		{"Wi-Fi iface unknown", NetSnapshot{DefaultIface: "en0", DefaultGateway: HotspotGateway, Iface: "en10"}, LinkOther},
	} {
		if got := tc.snap.Link(); got != tc.want {
			t.Errorf("%s: Link() = %s, want %s", tc.name, got, tc.want)
		}
	}
	for l, want := range map[NetLink]string{
		LinkHotspotUSB: "hotspot (USB)", LinkHotspotWiFi: "hotspot (Wi-Fi)", LinkWiFi: "Wi-Fi", LinkOther: "other", LinkNone: "none", "": "none",
	} {
		if got := l.Label(); got != want {
			t.Errorf("%q.Label() = %q, want %q", l, got, want)
		}
	}
}

func TestSleepTriggerAbortable(t *testing.T) {
	for trig, want := range map[SleepTrigger]bool{
		SleepTriggerSerious: true, SleepTriggerCPUTemp: true, SleepTriggerBatteryTemp: true,
		SleepTriggerCritical: false, SleepTriggerBatteryCritical: false, SleepTrigger("new"): false,
	} {
		if got := trig.Abortable(); got != want {
			t.Errorf("%s.Abortable() = %v, want %v", trig, got, want)
		}
	}
}
