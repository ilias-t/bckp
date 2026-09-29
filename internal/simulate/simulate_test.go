package simulate

import (
	"bytes"
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ilias-t/bckp/internal/config"
	"github.com/ilias-t/bckp/internal/contracts"
)

func TestParse(t *testing.T) {
	p, err := parse("for:90m hotspot nominal x4 fair ac:off batt:30 serious x2 extend:30 offline x3 net:fail fail cpu:96 poll wait:1m stop", 15*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	// 4 nominal + fair + 2 serious + 3 offline + net:fail + fail + poll + 4 (wait:1m)
	if len(p.polls) != 17 || p.forever || p.duration != 90*time.Minute || p.hotspot != config.HotspotOn {
		t.Fatalf("plan = %d polls %+v", len(p.polls), p)
	}
	if st := p.polls[4]; st.thermal.state != contracts.ThermalFair || !st.power.OnAC {
		t.Fatalf("poll 5 = %+v (settings must apply from the next poll)", st)
	}
	if st := p.polls[5]; st.power.OnAC || st.power.Percent != 30 || st.thermal.state != contracts.ThermalSerious {
		t.Fatalf("poll 6 = %+v", st)
	}
	if c := p.cmds[6]; len(c) != 1 || !c[0].extend || c[0].minutes != 30 {
		t.Fatalf("cmds after poll 7 = %+v", p.cmds)
	}
	if st := p.polls[10]; st.net.kind != "net:fail" || p.polls[11].net.kind != "offline" {
		t.Fatalf("net:fail must be one-off: %+v %+v", st.net, p.polls[11].net)
	}
	if !p.polls[11].thermal.fail || p.polls[12].thermal.fail || p.polls[12].thermal.state != contracts.ThermalSerious {
		t.Fatal("fail must be one-off and keep the thermal state")
	}
	if cpu := p.polls[12].thermal.cpu; cpu == nil || *cpu != 96 {
		t.Fatal("cpu must be sticky from the next poll")
	}
	if c := p.cmds[16]; len(c) != 1 || !c[0].stop || c[0].sleep != nil {
		t.Fatalf("final stop = %+v", p.cmds[16])
	}

	for _, bad := range []string{
		"", "x3", "nominal x0", "nominal x2 x2", "sersious", "batt:101", "nominal for:5m",
		"stop nominal", "nominal ac:off", "extend:0", "wait:-1m", "cpu:hot", "nominal cpu:95",
	} {
		if _, err := parse(bad, 15*time.Second); err == nil {
			t.Errorf("parse(%q) must fail", bad)
		}
	}
}

func run(t *testing.T, script string, mutate func(*config.Config)) string {
	t.Helper()
	cfg := config.Default()
	if mutate != nil {
		mutate(&cfg)
	}
	var out bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := Run(ctx, script, cfg, &out); err != nil {
		t.Fatalf("Run(%q): %v\n%s", script, err, out.String())
	}
	return out.String()
}

func mustContain(t *testing.T, out string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !regexp.MustCompile(w).MatchString(out) {
			t.Errorf("output missing /%s/:\n%s", w, out)
		}
	}
}

func TestSeriousScenario(t *testing.T) {
	out := run(t, "nominal x4 fair x3 serious x45 nominal x5", nil)
	mustContain(t, out,
		`57 polls every 15s`,
		`guard\s+acquire ttl 1m30s -> lease-1`,
		`\+0:01:15\s+#6\s+fair\s+FAIR`,
		`notify\s+p3 Mac warming up \(fair\)`,
		`\+0:01:45\s+#8\s+serious\s+HOT`,
		`#9-47\s+serious \(x39, quiet\)\s+HOT`,
		// The 41st serious poll (#48) is 10m after HOT began.
		`\+0:11:45\s+#48\s+serious\s+SLEEP_PENDING \(serious in 60s, abortable\)`,
		`notify\s+p5 Sleeping in 1m: hot for 10m`,
		`\+0:12:45\s+thermal\s+SLEEP_PENDING -> SLEEPING`,
		`guard\s+sleepnow "thermal serious: hot for 10m"`,
		`ended\s+thermal_sleep at \+0:12:45`,
		`unused\s+6 of 57 script polls never ran \(#52-57\)`,
		`release x0; nothing slept`,
	)
}

func TestCriticalScenario(t *testing.T) {
	out := run(t, "nominal x2 serious x3 critical critical stop:nosleep extend:30 critical", nil)
	mustContain(t, out,
		`#6\s+critical\s+HOT`,
		`notify\s+p5 Mac CRITICAL heat - take it out now`,
		`#7\s+critical\s+SLEEP_PENDING \(critical in 15s, not abortable\)`,
		`>\s+bckp stop --no-sleep`,
		`refused: not_abortable`,
		`>\s+bckp extend 30`,
		`guard\s+sleepnow "thermal critical: critical heat for 2 polls"`,
		`ended\s+thermal_sleep at \+0:01:45`,
	)
}

func TestExtendAbortsSeriousSleep(t *testing.T) {
	out := run(t, "for:30m serious x41 extend:30 serious x2 stop", nil)
	mustContain(t, out,
		`pending serious sleep cancelled by extend`,
		`notify\s+p4 Thermal sleep cancelled`,
		`extended: until 10:00`,
		`ended\s+stopped`,
		`sleepnow x0, release x1`,
	)
}

func TestPendingSleepPlaysOutAfterScript(t *testing.T) {
	out := run(t, "serious x41", nil)
	mustContain(t, out, `end of script`, `ended\s+thermal_sleep at \+0:11:00`, `extra\s+3 polls past the script`)
}

func TestScriptEndStops(t *testing.T) {
	out := run(t, "nominal x3", nil)
	mustContain(t, out, `end of script`, `>\s+bckp stop`, `notify\s+p3 backpack off`, `guard\s+release lease-1`, `ended\s+stopped`)
}

func TestPowerAndNetworkScenario(t *testing.T) {
	out := run(t, "ac:off batt:40 poll offline x8 online batt:24 poll", nil)
	mustContain(t, out,
		`#1\s+nominal \| battery 40% \| online`,
		`#2\s+nominal \| offline`,
		`network\s+offline`,
		`\+0:01:45\s+#8\s+nominal`,
		`net\s+service "iPhone USB" off`,
		`\+0:01:48\s+net\s+service "iPhone USB" on`,
		`notify\s+p3 Mac back online after`,
		`notify\s+p5 Battery 24% - backpack off`,
		`ended\s+battery_floor`,
	)
}

func TestACToBattery(t *testing.T) {
	out := run(t, "nominal x2 ac:off batt:80 nominal x2", nil)
	mustContain(t, out, `#3\s+nominal \| battery 80%`, `notify\s+p4 On battery \(80%\)`)
}

func TestLeaseLost(t *testing.T) {
	out := run(t, "nominal x2 lease:lost nominal x3", nil)
	mustContain(t, out, `>\s+daemon lease dropped`, `lease\s+lease lost`, `notify\s+p5 Sleep guard lost`, `ended\s+lease_lost`)
}

func TestHotspotScenario(t *testing.T) {
	out := run(t, "hotspot online x3 stop", nil)
	mustContain(t, out,
		`prefer hotspot "iPhone USB"; watchdog on`,
		`net\s+set service order \["iPhone USB" "Wi-Fi" "Thunderbolt Bridge"\]`,
		`network\s+moved "iPhone USB" ahead of Wi-Fi: iPhone USB, Wi-Fi, Thunderbolt Bridge`,
		`net\s+set service order \["Wi-Fi" "iPhone USB" "Thunderbolt Bridge"\]`,
		`network\s+network restored from journal`,
	)
	if strings.Contains(out, "wifi off") {
		t.Fatalf("the hotspot preference must not touch Wi-Fi:\n%s", out)
	}
}

func TestCustomServiceName(t *testing.T) {
	out := run(t, "hotspot offline x8 stop", func(c *config.Config) { c.Network.Service = "iPhone 15 USB" })
	mustContain(t, out, `set service order \["iPhone 15 USB" "Wi-Fi"`, `net\s+service "iPhone 15 USB" off`)
	if strings.Contains(out, "Hotspot preference off") {
		t.Fatalf("a configured service name must work:\n%s", out)
	}
}

func TestMaxDurationScenario(t *testing.T) {
	out := run(t, "wait:1h poll", func(c *config.Config) {
		c.Session.MaxDuration = time.Hour
		c.Notify.Heartbeat = 20 * time.Minute
	})
	mustContain(t, out,
		`\+0:20:00\s+#81[^\n]*\n\s+notify\s+p2 Mac: nominal`,
		`\+0:50:00\s+notify\s+p4 backpack ends in 10m \(1h cap\)`,
		`\+1:00:00\s+session\s+session ending: max_duration`,
		`ended\s+max_duration at \+1:00:00`,
	)
}

func TestCPUSleepScenario(t *testing.T) {
	out := run(t, "cpu:90 nominal x2 cpu:97 nominal x2 stop:nosleep", func(c *config.Config) {
		v := 95.0
		c.Thermal.CPUSleepC = &v
	})
	mustContain(t, out, `cpu_sleep_c 95°C`, `SLEEP_PENDING \(cpu_temp in 60s, abortable\)`, `ended\s+stopped`, `sleepnow x0`)
}

func TestDeterministic(t *testing.T) {
	a := run(t, "nominal x4 fair x3 serious x45 nominal x5", nil)
	b := run(t, "nominal x4 fair x3 serious x45 nominal x5", nil)
	if a != b {
		t.Fatal("simulate output must be deterministic")
	}
}

func TestErrors(t *testing.T) {
	var out bytes.Buffer
	// The CLI prefixes "simulate: " itself.
	if err := Run(context.Background(), "sersious x3", config.Default(), &out); !errors.Is(err, contracts.ErrBadScript) ||
		err.Error() != `bad script: token 1 "sersious": unknown token` {
		t.Fatalf("parse error = %v", err)
	}
	bad := config.Default()
	bad.Session.Poll = 0
	if err := Run(context.Background(), "nominal", bad, &out); err == nil {
		t.Fatal("invalid config must fail")
	}
}

func TestHotspotAutoPlugUnplug(t *testing.T) {
	out := run(t, "hotspot:auto unplug online x2 plug online x2 unplug online x2 stop", nil)
	mustContain(t, out,
		`prefer hotspot "iPhone USB" once connected \(auto\); watchdog on`,
		// Engaged on the first poll with the phone plugged in (poll 3)...
		`\+0:00:30\s+#3[^\n]*phone plugged in[^\n]*\n\s+network\s+online: Wi-Fi -> hotspot \(USB\)\n\s+net\s+set service order \["iPhone USB" "Wi-Fi" "Thunderbolt Bridge"\]`,
		`network\s+hotspot connected: moved "iPhone USB" ahead of Wi-Fi`,
		`#1\s+nominal \| AC 100% \| online \(phone unplugged\)`,
		// ...and restored at the end.
		`net\s+set service order \["Wi-Fi" "iPhone USB" "Thunderbolt Bridge"\]`,
	)
	if n := strings.Count(out, "set service order"); n != 2 {
		t.Fatalf("want one reorder and one restore (unplug changes nothing), got %d:\n%s", n, out)
	}
}

func TestHotspotAndWatchdogTokens(t *testing.T) {
	p, err := parse("hotspot:off watchdog:off plug nominal", 15*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if p.hotspot != config.HotspotOff || p.watchdog == nil || *p.watchdog {
		t.Fatalf("plan = %+v", p)
	}
	for _, bad := range []string{"hotspot:maybe nominal", "watchdog:yes nominal", "nominal hotspot:on"} {
		if _, err := parse(bad, 15*time.Second); err == nil {
			t.Errorf("%q: expected an error", bad)
		}
	}
	out := run(t, "hotspot:off watchdog:off nominal x2", nil)
	mustContain(t, out, `network\s+off`)
}

func TestWiFiNudgeTimeline(t *testing.T) {
	out := run(t, "hotspot:on wifi:other x4 wifi:none x18 wifi:hotspot x2 stop", nil)
	mustContain(t, out,
		`wifi nudge after 1m offline without USB, at most every 3m \(lands within 45s\)`,
		`#1\s+nominal \| AC 100% \| wifi:other \(phone unplugged\)`,
		// Offline from #5 (1:00); first nudge 60s later.
		`\+0:02:00\s+#9[^\n]*\n\s+network\s+wifi nudge 1: Wi-Fi off \(offline 1m\)\n\s+net\s+wifi off\n\s+\+0:02:03\s+net\s+wifi on`,
		`wifi nudge 1: still_offline \(still offline after 57s\)`,
		// At most once every 3m.
		`\+0:05:00\s+#21[^\n]*\n\s+network\s+wifi nudge 2: Wi-Fi off \(offline 4m\)`,
		`wifi nudge 2: landed_hotspot \(landed on the hotspot \(Wi-Fi\) after 27s\)`,
		`notify\s+p3 Mac back online after 4m30s via hotspot \(Wi-Fi\)`,
	)
	if n := strings.Count(out, "Wi-Fi off ("); n != 2 {
		t.Fatalf("want 2 nudges, got %d:\n%s", n, out)
	}
	if strings.Contains(out, "nudge 3") {
		t.Fatalf("nudged after landing:\n%s", out)
	}
}

func TestWiFiNudgeNeverWhileOnlineElsewhere(t *testing.T) {
	out := run(t, "hotspot:on wifi:other x40 stop", nil)
	// Only the header mentions the nudge.
	if strings.Count(out, "nudge") != 1 {
		t.Fatalf("nudged while online:\n%s", out)
	}
}

func TestWiFiNudgeAutoNeverNudges(t *testing.T) {
	out := run(t, "hotspot:auto online x2 wifi:none x30 stop", nil)
	if strings.Contains(out, "nudge") {
		t.Fatalf("auto nudged:\n%s", out)
	}
}

func TestParseWiFiTokens(t *testing.T) {
	p, err := parse("plug online wifi:hotspot wifi:other wifi:none online", 15*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []netInput{
		{kind: "online"}, {kind: "wifi:hotspot", unplugged: true}, {kind: "wifi:other", unplugged: true},
		{kind: "wifi:none", unplugged: true}, {kind: "online", unplugged: true},
	} {
		if p.polls[i].net != want {
			t.Errorf("poll %d: %+v, want %+v", i+1, p.polls[i].net, want)
		}
	}
}

// TestPushMarkings: every notification line says whether the push policy
// (notify.push, default "attention") pushes it or only logs it, and the
// summary counts both.
func TestPushMarkings(t *testing.T) {
	out := run(t, "hotspot:on serious x2 nominal x3 wifi:none x30 wifi:hotspot x2 stop", nil)
	mustContain(t, out,
		`notify\s+p4 Mac is HOT \(serious\) - open the bag \[pushed\]`,
		`notify\s+p3 backpack on \(forever\) \[log only\]`,
		`notify\s+p3 Mac cooled down \(nominal\) \[pushed\]`,
		// Held one poll, then pushed: the outage passed notify.outage_min.
		`notify\s+p3 Mac back online after 7m30s via hotspot \(Wi-Fi\) \[pushed\]\n\s+#\d+`,
		`notify\s+p3 backpack off \[log only\]`,
		`notified  6: 3 pushed, 3 log only \(notify.push = "attention"; p4 x1, p3 x4, p2 x1\)`,
	)
}

// TestHeaderLegendAndPlurals: the header explains the columns, and counts
// read right for one.
func TestHeaderLegendAndPlurals(t *testing.T) {
	out := run(t, "nominal", nil)
	mustContain(t, out, `legend\s+#N: script poll N \| pN: notification priority`, `\n\s+1 poll every 15s`)
	if strings.Contains(out, "1 polls") {
		t.Fatalf("plural:\n%s", out)
	}
	if !strings.Contains(Grammar, "wifi:hotspot") || !strings.Contains(Grammar, "lease:lost") {
		t.Fatal("Grammar must list every token")
	}
}
