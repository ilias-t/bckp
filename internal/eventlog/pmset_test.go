package eventlog

import (
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"
)

// testdata/pmset_log.txt is real `pmset -g log` output (macOS 27.0, Apple
// Silicon) around lid-closed sleeps, with UUIDs zeroed, trimmed: most
// Assertions lines and app-specific lines removed. It keeps the header,
// "Sleep/Wakes since boot" lines, Wake Requests / Client Acks lines whose
// messages mention Sleep and Wake, the powerd Start and a ThermalEvent,
// and the trailing assertion listing.
func loadPmsetFixture(t *testing.T) []Event {
	t.Helper()
	f, err := os.Open("testdata/pmset_log.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	evs, err := ParsePmsetLog(f, time.Time{}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	return evs
}

func TestParsePmsetLogFixture(t *testing.T) {
	evs := loadPmsetFixture(t)
	counts := map[string]int{}
	for _, ev := range evs {
		counts[ev.Name]++
		if ev.Src != SrcMacOS {
			t.Errorf("src = %q", ev.Src)
		}
	}
	want := map[string]int{EventPMSleep: 10, EventPMDarkWake: 5, EventPMWake: 3, EventPMThermal: 1, EventPMBoot: 1}
	for k, n := range want {
		if counts[k] != n {
			t.Errorf("%s: %d events, want %d (all counts %v)", k, counts[k], n, counts)
		}
	}
	if len(evs) != 20 {
		t.Fatalf("%d events, want 20 (Wake Requests / Client Acks must not match)", len(evs))
	}
	for i := 1; i < len(evs); i++ {
		if evs[i].Time.Before(evs[i-1].Time) {
			t.Fatalf("events out of order at %d", i)
		}
	}

	loc := time.FixedZone("EDT", -4*3600)
	find := func(clock string) Event {
		t.Helper()
		for _, ev := range evs {
			if ev.Time.In(loc).Format("15:04:05") == clock {
				return ev
			}
		}
		t.Fatalf("no event at %s", clock)
		return Event{}
	}

	// The clamshell sleep during the 17:23 incident.
	ev := find("17:23:51")
	if ev.Name != EventPMSleep || ev.Level != slog.LevelWarn {
		t.Fatalf("17:23:51 = %+v", ev)
	}
	for k, v := range map[string]string{"state": "Sleep", "reason": "Clamshell Sleep", "power": "AC", "charge": "100", "secs": "223"} {
		if got, _ := ev.Get(k); plain(got) != v {
			t.Errorf("17:23:51 %s = %v, want %s", k, got, v)
		}
	}
	if !ev.Time.Equal(time.Date(2026, 9, 28, 17, 23, 51, 0, loc)) {
		t.Errorf("time = %v", ev.Time)
	}

	cases := map[string]string{
		"17:21:38": "17:21:38 macos   pm.sleep            WARN DarkWake due to 'Clamshell Sleep' · AC 100%",
		"17:23:06": "17:23:06 macos   pm.darkwake         DarkWake from Deep Idle: HID Activity · AC 100% · 45s",
		"18:30:05": "18:30:05 macos   pm.wake             DarkWake to FullWake from Deep Idle: Notification · AC 100% · 1s",
		"18:31:26": "18:31:26 macos   pm.sleep            Sleep due to 'Maintenance Sleep' · AC 100% · 132s",
		"18:41:17": "18:41:17 macos   pm.wake             Wake from Deep Idle: smc.sysState.Wake(0x70070000) lid SMC.OutboxNotEmpty nub-spmi-a0.0x0B RTP.multi-touch/UserActivity Assertion · AC 100% · 134s",
		"15:11:23": "15:11:23 macos   pm.thermal          WARN Ignored DarkWake thermal emergency signal TCPKeepAlive=active",
		"16:58:19": "16:58:19 macos   pm.boot             powerd started (boot)",
	}
	for clock, want := range cases {
		if got := Format(find(clock), loc); got != want {
			t.Errorf("%s:\n got %q\nwant %q", clock, got, want)
		}
	}
}

func TestParsePmsetLogWindowAndBattery(t *testing.T) {
	loc := time.FixedZone("EDT", -4*3600)
	f, err := os.Open("testdata/pmset_log.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	since := time.Date(2026, 9, 28, 17, 23, 0, 0, loc)
	until := time.Date(2026, 9, 28, 17, 24, 0, 0, loc)
	evs, err := ParsePmsetLog(f, since, until)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 3 { // 17:23:05 sleep, 17:23:06 darkwake, 17:23:51 sleep
		t.Fatalf("window: %d events: %v", len(evs), evs)
	}

	in := "2026-09-28 18:29:47 -0400 Sleep               \tEntering Sleep state due to 'Maintenance Sleep':TCPKeepAlive=active Using Batt (Charge:42%) 2 secs    \n" +
		"2026-09-28 18:29:49 -0400 DarkWake            \tDarkWake from Deep Idle [CDNP] : due to smc.sysState.Wake(0x70070000) wifibt SMC.OutboxNotEmpty/ Using BATT (Charge:42%) 16 secs   \n" +
		"2026-09-28 18:29:48 -0400 Wake Requests       \t[*process=dasd request=SleepService deltaSecs=1036]\n" +
		"Sleep/Wakes since boot at 2026-09-28 16:58:19 -0400 :10   Dark Wake Count in this sleep cycle:10\n" +
		"2026-09-28 21:24:56 -0400 : Showing all currently held IOKit power assertions\n" +
		"2026-09-28 18:30:00 -0400 Wake"
	evs, err = ParsePmsetLog(strings.NewReader(in), time.Time{}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 2 {
		t.Fatalf("%d events: %v", len(evs), evs)
	}
	if got := Format(evs[0], loc); got != "18:29:47 macos   pm.sleep            Sleep due to 'Maintenance Sleep' · battery 42% · 2s" {
		t.Errorf("battery sleep: %q", got)
	}
	if got := Summary(evs[1]); got != "DarkWake from Deep Idle: smc.sysState.Wake(0x70070000) wifibt SMC.OutboxNotEmpty · battery 42% · 16s" {
		t.Errorf("battery darkwake: %q", got)
	}
}
