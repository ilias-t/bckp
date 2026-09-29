package tui

import (
	"bytes"
	"context"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"

	"github.com/ilias-t/bckp/internal/contracts"
)

var update = flag.Bool("update", false, "rewrite testdata/*.golden")

var base = time.Date(2026, 9, 28, 21, 0, 0, 0, time.UTC)

func fp(v float64) *float64 { return &v }

func nominalSnap() contracts.Snapshot {
	return contracts.Snapshot{
		At:  base,
		PID: 4242,
		Session: contracts.SessionStatus{
			StartedAt:  base.Add(-7 * time.Minute),
			EndsAt:     base.Add(83 * time.Minute),
			CapAt:      base.Add(12*time.Hour - 7*time.Minute),
			RemainingS: 83 * 60,
		},
		Thermal: contracts.ThermalStatus{
			FSM: contracts.FSMNominal,
			Reading: &contracts.ThermalReading{
				State: contracts.ThermalNominal, CPUC: fp(52.3), GPUC: fp(41.0), At: base,
			},
		},
		Power: &contracts.PowerReading{
			OnAC: true, Percent: 87, Charging: true, LidClosed: true, SleepDisabled: true, At: base,
		},
		Network: contracts.NetworkStatus{
			Mode:   contracts.NetworkPreferHotspot,
			Online: true,
			Latest: &contracts.NetSnapshot{
				DefaultIface: "en10", Iface: "en10", Gateway: "172.20.10.1", Online: true,
				IfacePresent: true, GatewayReachable: true, IPhoneUSBPresent: true, WiFiOn: true, At: base,
			},
		},
		Lease: contracts.LeaseStatus{Held: true, ID: "l-1", Expires: base.Add(80 * time.Second)},
		Notifications: []contracts.NotificationRecord{
			{Notification: contracts.Notification{Priority: 2, Title: "backpack started", Body: "90m, prefer hotspot"}, At: base.Add(-7 * time.Minute), Status: contracts.NotificationSent},
			{Notification: contracts.Notification{Priority: 3, Title: "Heartbeat", Body: "nominal, 52°C, AC 87%"}, At: base.Add(-1 * time.Minute), Status: contracts.NotificationSent},
		},
	}
}

func seriousSnap() contracts.Snapshot {
	s := nominalSnap()
	s.Session.Forever = true
	s.Session.EndsAt = time.Time{}
	s.Session.RemainingS = int64((12*time.Hour - 7*time.Minute) / time.Second)
	s.Thermal = contracts.ThermalStatus{
		FSM:      contracts.FSMSleepPending,
		Reading:  &contracts.ThermalReading{State: contracts.ThermalSerious, CPUC: fp(88.4), GPUC: fp(71.2), At: base},
		HotSince: base.Add(-10*time.Minute - 5*time.Second),
	}
	s.SleepPending = &contracts.SleepPending{
		Trigger: contracts.SleepTriggerSerious, Reason: "serious heat for 10m",
		At: base.Add(45 * time.Second), RemainingS: 45, Abortable: true,
	}
	s.Notifications = []contracts.NotificationRecord{
		{Notification: contracts.Notification{Priority: 2, Title: "backpack started", Body: "forever, prefer hotspot"}, At: base.Add(-30 * time.Minute), Status: contracts.NotificationSent},
		{Notification: contracts.Notification{Priority: 3, Title: "Thermal fair", Body: "CPU 71°C"}, At: base.Add(-12 * time.Minute), Status: contracts.NotificationSent},
		{Notification: contracts.Notification{Priority: 4, Title: "Thermal serious", Body: "CPU 84°C"}, At: base.Add(-10*time.Minute - 5*time.Second), Status: contracts.NotificationSent},
		{Notification: contracts.Notification{Priority: 4, Title: "Still hot", Body: "serious for 10m"}, At: base.Add(-20 * time.Second), Status: contracts.NotificationSent},
		{Notification: contracts.Notification{Priority: 5, Title: "Sleeping in 60s", Body: "serious heat for 10m\nextend to abort"}, At: base.Add(-15 * time.Second), Status: contracts.NotificationFailed},
		{Notification: contracts.Notification{Priority: 3, Title: "Heartbeat", Body: "serious, 88°C, AC 87%"}, At: base.Add(-5 * time.Second), Status: contracts.NotificationSent},
	}
	return s
}

func offlineSnap() contracts.Snapshot {
	s := nominalSnap()
	s.Network.Online = false
	s.Network.OfflineSince = base.Add(-3*time.Minute - 12*time.Second)
	s.Network.Recoveries = 2
	s.Network.LastRecovery = base.Add(-1 * time.Minute)
	s.Network.Latest.Online = false
	s.Network.Latest.DefaultIface = ""
	s.Network.Latest.GatewayReachable = false
	s.Network.Latest.WiFiOn = false
	s.OutboxPending = 2
	s.Notifications = append(s.Notifications,
		contracts.NotificationRecord{Notification: contracts.Notification{Priority: 4, Title: "Hotspot offline", Body: "no default route"}, At: base.Add(-3 * time.Minute), Status: contracts.NotificationQueued},
	)
	s.Errors = map[string]string{"network": "probe: dial tcp 1.1.1.1:443: i/o timeout", "notify": "ntfy: no route to host"}
	return s
}

func noTempsSnap() contracts.Snapshot {
	s := nominalSnap()
	s.Thermal.Reading.CPUC, s.Thermal.Reading.GPUC = nil, nil
	s.Power = nil
	s.Network.Latest = nil
	s.Lease = contracts.LeaseStatus{}
	s.Notifications = nil
	return s
}

func cpuHistory(from, to float64, n int) []float64 {
	h := make([]float64, n)
	for i := range h {
		h[i] = from + (to-from)*float64(i)/float64(n-1)
	}
	return h
}

// testModel is a live model showing s at base, with a fixed clock.
func testModel(s *contracts.Snapshot, width int, p termenv.Profile, hist []float64) model {
	m := newModel(context.Background(), nil, nil, nil, newStyles(p))
	m.loc = time.UTC
	m.clock = func() time.Time { return base }
	m.now, m.recvAt = base, base
	m.width = width
	m.snap = s
	m.history = hist
	if s != nil {
		m.conn = connLive
	}
	return m
}

func checkGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name+".golden")
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden (run with -update to create): %v", err)
	}
	if got != string(want) {
		t.Errorf("%s mismatch\n--- got ---\n%s\n--- want ---\n%s", name, got, want)
	}
}

func checkWidth(t *testing.T, out string, w int) {
	t.Helper()
	for i, l := range strings.Split(out, "\n") {
		if n := ansi.StringWidth(l); n > w {
			t.Errorf("line %d is %d cols wide, max %d: %q", i+1, n, w, l)
		}
	}
}

func TestRenderGolden(t *testing.T) {
	serious := seriousSnap()
	nominal := nominalSnap()
	offline := offlineSnap()
	noTemps := noTempsSnap()
	critical := seriousSnap()
	critical.Thermal.Reading.State = contracts.ThermalCritical
	critical.Thermal.Reading.CPUC = fp(101.5)
	critical.SleepPending = &contracts.SleepPending{
		Trigger: contracts.SleepTriggerCritical, Reason: "critical x2", At: base.Add(15 * time.Second), RemainingS: 15,
	}
	ended := nominalSnap()
	ended.Session.Ended = true
	ended.Session.EndReason = contracts.EndThermalSleep
	ended.Lease = contracts.LeaseStatus{}

	tests := []struct {
		name  string
		snap  *contracts.Snapshot
		width int
		hist  []float64
		setup func(*model)
	}{
		{name: "nominal", snap: &nominal, width: 80, hist: cpuHistory(48, 56, 24)},
		{name: "serious_countdown", snap: &serious, width: 80, hist: cpuHistory(60, 88.4, 40)},
		{name: "critical_countdown", snap: &critical, width: 80, hist: cpuHistory(80, 101.5, 40)},
		{name: "offline_outage", snap: &offline, width: 80, hist: cpuHistory(50, 52, 8)},
		{name: "no_temps", snap: &noTemps, width: 80},
		{name: "narrow_60", snap: &serious, width: 60, hist: cpuHistory(60, 88.4, 40)},
		{name: "narrow_offline_60", snap: &offline, width: 60, hist: cpuHistory(50, 52, 8)},
		{name: "ended", snap: &ended, width: 80, setup: func(m *model) { m.conn = connEnded }},
		{name: "connecting", width: 80, setup: func(m *model) { m.conn = connConnecting }},
		{name: "connection_lost", snap: &nominal, width: 80, setup: func(m *model) {
			m.conn, m.attempt, m.retryAt = connLost, 3, base.Add(4*time.Second)
			m.lastErr = "engine not running"
		}},
		{name: "confirm_stop", snap: &serious, width: 80, setup: func(m *model) { m.confirmStop = true }},
		{name: "flash_not_abortable", snap: &critical, width: 80, setup: func(m *model) {
			m.flash, m.flashErr = notAbortableText, true
		}},
		{name: "help", snap: &nominal, width: 80, setup: func(m *model) { m.showHelp = true }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := testModel(tc.snap, tc.width, termenv.Ascii, tc.hist)
			if tc.setup != nil {
				tc.setup(&m)
			}
			out := render(m)
			if strings.Contains(out, "\x1b") {
				t.Errorf("Ascii render contains escape sequences")
			}
			checkWidth(t, out, tc.width)
			checkGolden(t, tc.name, out)
		})
	}
}

func TestRenderCountdownTicksLocally(t *testing.T) {
	s := seriousSnap()
	m := testModel(&s, 80, termenv.Ascii, nil)
	m.now = base.Add(40 * time.Second)
	out := render(m)
	if !strings.Contains(out, "sleep pending 0:05") || !strings.Contains(out, "sleep in 0:05") {
		t.Errorf("countdown not advanced by local elapsed time:\n%s", out)
	}
	if !strings.Contains(out, "hot 11m") { // 10m05s + 40s, rounded to the minute
		t.Errorf("hot duration not advanced:\n%s", out)
	}
	m.now = base.Add(2 * time.Minute)
	if out := render(m); !strings.Contains(out, "sleep pending 0:00") {
		t.Errorf("countdown not floored at 0:\n%s", out)
	}
}

func TestRenderNoColorEnv(t *testing.T) {
	env := fakeEnv{"TERM": "xterm-256color", "NO_COLOR": "1"}
	p := detectProfile(&bytes.Buffer{}, termenv.WithTTY(true), termenv.WithEnvironment(env))
	if p != termenv.Ascii {
		t.Fatalf("NO_COLOR profile = %v, want Ascii", p)
	}
	s := seriousSnap()
	m := testModel(&s, 80, p, cpuHistory(60, 88.4, 40))
	out := render(m)
	if strings.Contains(out, "\x1b") {
		t.Errorf("NO_COLOR render contains escape sequences:\n%q", out)
	}
	if !strings.Contains(out, "[SERIOUS]") {
		t.Errorf("NO_COLOR badge should be bracketed text:\n%s", out)
	}
	checkGolden(t, "no_color", out)
}

func TestRenderColor(t *testing.T) {
	env := fakeEnv{"TERM": "xterm-256color"}
	p := detectProfile(&bytes.Buffer{}, termenv.WithTTY(true), termenv.WithEnvironment(env))
	if p == termenv.Ascii {
		t.Fatalf("color terminal detected as Ascii")
	}
	for _, tc := range []struct {
		state contracts.ThermalState
		code  string // 256-color background
	}{
		{contracts.ThermalNominal, "48;5;34"},
		{contracts.ThermalFair, "48;5;220"},
		{contracts.ThermalSerious, "48;5;208"},
		{contracts.ThermalCritical, "48;5;196"},
	} {
		s := nominalSnap()
		s.Thermal.Reading.State = tc.state
		m := testModel(&s, 80, termenv.ANSI256, nil)
		out := render(m)
		name := strings.ToUpper(tc.state.String())
		if !strings.Contains(out, tc.code) || !strings.Contains(out, " "+name+" ") {
			t.Errorf("%s badge: want background %s, got:\n%q", name, tc.code, out)
		}
		if strings.Contains(out, "["+name+"]") {
			t.Errorf("%s badge: color mode should not use brackets", name)
		}
		checkWidth(t, out, 80)
	}
	s := nominalSnap()
	checkGolden(t, "nominal_color", render(testModel(&s, 80, termenv.ANSI256, cpuHistory(48, 56, 24))))
}

func TestSparkline(t *testing.T) {
	if got := sparkline([]float64{1, 2, 3, 4, 5, 6, 7, 8}); got != "▁▂▃▄▅▆▇█" {
		t.Errorf("ramp = %q", got)
	}
	if got := sparkline([]float64{50, 50, 50}); got != "▅▅▅" {
		t.Errorf("flat = %q", got)
	}
}

type fakeEnv map[string]string

func (e fakeEnv) Environ() []string {
	var out []string
	for k, v := range e {
		out = append(out, k+"="+v)
	}
	return out
}

func (e fakeEnv) Getenv(k string) string { return e[k] }

func TestRenderNetworkHotspotOverWiFi(t *testing.T) {
	s := nominalSnap()
	s.Network.Latest = &contracts.NetSnapshot{DefaultIface: "en0", DefaultGateway: contracts.HotspotGateway, WiFiIface: "en0",
		Iface: "en10", Online: true, WiFiOn: true, At: base}
	s.Network.Link = contracts.LinkHotspotWiFi
	s.Network.WiFiNudges, s.Network.LastWiFiNudge, s.Network.LastWiFiNudgeResult = 2, base.Add(-2*time.Minute), "landed_hotspot"
	got := strings.Join(renderNetwork(testModel(&s, 120, termenv.Ascii, nil), 120), "\n")
	for _, want := range []string{"online via hotspot (Wi-Fi)", "no iPhone USB", "Wi-Fi nudges 2 (last 20:58 landed_hotspot)"} {
		if !strings.Contains(got, want) {
			t.Errorf("network lacks %q:\n%s", want, got)
		}
	}
}

// TestRenderLoggedNotification: info notifications the push policy only
// logged are marked, so the list tells them from pushed ones.
func TestRenderLoggedNotification(t *testing.T) {
	s := nominalSnap()
	s.Notifications = []contracts.NotificationRecord{
		{Notification: contracts.Notification{Priority: 3, Title: "backpack on (forever)"}, At: base.Add(-time.Minute), Status: contracts.NotificationLogged},
		{Notification: contracts.Notification{Priority: 4, Title: "Mac is HOT (serious) - open the bag"}, At: base, Status: contracts.NotificationSent},
	}
	got := strings.Join(renderNotifications(testModel(&s, 120, termenv.Ascii, nil), 120), "\n")
	if !strings.Contains(got, "p3 (log only) backpack on (forever)") || strings.Contains(got, "p4 (log only)") {
		t.Fatalf("notifications:\n%s", got)
	}
}
