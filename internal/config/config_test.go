package config

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/ilias-t/bckp/internal/paths"
)

func TestDefaultsAreValid(t *testing.T) {
	cfg := Default()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("defaults invalid: %v", err)
	}
	checks := []struct {
		name      string
		got, want any
	}{
		{"poll", cfg.Session.Poll, 15 * time.Second},
		{"max_duration", cfg.Session.MaxDuration, 12 * time.Hour},
		{"max_duration_warning", cfg.Session.MaxDurationWarning, 10 * time.Minute},
		{"fair_polls", cfg.Thermal.FairPolls, 2},
		{"fair_cooldown", cfg.Thermal.FairCooldown, 30 * time.Minute},
		{"hot_renotify", cfg.Thermal.HotRenotify, 10 * time.Minute},
		{"serious_sleep_after", cfg.Thermal.SeriousSleepAfter, 10 * time.Minute},
		{"serious_grace", cfg.Thermal.SeriousGrace, 60 * time.Second},
		{"critical_polls", cfg.Thermal.CriticalPolls, 2},
		{"critical_grace", cfg.Thermal.CriticalGrace, 15 * time.Second},
		{"cooled_polls", cfg.Thermal.CooledPolls, 2},
		{"nominal_polls", cfg.Thermal.NominalPolls, 3},
		{"cpu_sleep_c", cfg.Thermal.CPUSleepC == nil, true},
		{"battery_floor", cfg.Power.BatteryFloor, 25},
		{"notify_on_battery", cfg.Power.NotifyOnBattery, true},
		{"watchdog", cfg.Network.Watchdog, true},
		{"recovery_after", cfg.Network.RecoveryAfter, 90 * time.Second},
		{"recovery_min_interval", cfg.Network.RecoveryMinInterval, 180 * time.Second},
		{"service", cfg.Network.Service, "iPhone USB"},
		{"prefer_hotspot", cfg.Network.PreferHotspot, HotspotAuto},
		{"wifi_nudge_after", cfg.Network.WiFiNudgeAfter, 60 * time.Second},
		{"wifi_nudge_every", cfg.Network.WiFiNudgeEvery, 3 * time.Minute},
		{"ntfy_server", cfg.Notify.NtfyServer, "https://ntfy.sh"},
		{"ntfy_topic", cfg.Notify.NtfyTopic, ""},
		{"heartbeat", cfg.Notify.Heartbeat, time.Duration(0)},
		{"push", cfg.Notify.Push, PushAttention},
		{"macos", cfg.Notify.MacOS, true},
		{"outage_min", cfg.Notify.OutageMin, 5 * time.Minute},
		{"log.max_mb", cfg.Log.MaxMB, 50},
		{"log.max_age", cfg.Log.MaxAge, 168 * time.Hour},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
}

func writeFile(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadFileOverridesDefaults(t *testing.T) {
	p := writeFile(t, `
[session]
poll = "10s"
max_duration = "2h"

[thermal]
cpu_sleep_c = 97.5
critical_polls = 3

[network]
service = "iPhone USB 2"
iface = "en10"

[notify]
ntfy_topic = "my-topic_1"
`)
	cfg, err := Load(LoadOptions{Path: p, MustExist: true})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Session.Poll != 10*time.Second || cfg.Session.MaxDuration != 2*time.Hour {
		t.Errorf("session = %+v", cfg.Session)
	}
	if cfg.Thermal.CPUSleepC == nil || *cfg.Thermal.CPUSleepC != 97.5 || cfg.Thermal.CriticalPolls != 3 {
		t.Errorf("thermal = %+v", cfg.Thermal)
	}
	// Untouched keys keep defaults.
	if cfg.Thermal.FairPolls != 2 || cfg.Session.MaxDurationWarning != 10*time.Minute {
		t.Errorf("defaults lost: %+v", cfg)
	}
	if cfg.Network.Service != "iPhone USB 2" || cfg.Network.Iface != "en10" {
		t.Errorf("network = %+v", cfg.Network)
	}
	if cfg.Notify.NtfyTopic != "my-topic_1" {
		t.Errorf("topic = %q", cfg.Notify.NtfyTopic)
	}
}

func TestLoadMissingFile(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope.toml")
	cfg, err := Load(LoadOptions{Path: missing})
	if err != nil {
		t.Fatalf("missing default file should load defaults: %v", err)
	}
	if cfg.Session.Poll != 15*time.Second {
		t.Errorf("poll = %s", cfg.Session.Poll)
	}
	if _, err := Load(LoadOptions{Path: missing, MustExist: true}); err == nil {
		t.Fatal("missing explicit file should fail")
	}
}

func TestLoadRejectsUnknownKeys(t *testing.T) {
	p := writeFile(t, "[thermal]\nfair_pols = 3\n")
	_, err := Load(LoadOptions{Path: p})
	if err == nil || !strings.Contains(err.Error(), "thermal.fair_pols") {
		t.Fatalf("err = %v, want unknown key thermal.fair_pols", err)
	}
}

func TestLoadRejectsBadSyntax(t *testing.T) {
	p := writeFile(t, "[session]\npoll = \"15 parsecs\"\n")
	if _, err := Load(LoadOptions{Path: p}); err == nil {
		t.Fatal("expected duration parse error")
	}
}

func TestEnvOverrides(t *testing.T) {
	p := writeFile(t, "[notify]\nntfy_topic = \"from-file\"\n[thermal]\ncpu_sleep_c = 90\n")
	env := []string{
		"PATH=/usr/bin",
		"BACKPACK_CONFIG=/ignored",
		"BACKPACK_E2E=1",
		"BACKPACK_SESSION_POLL=5s",            // duration
		"BACKPACK_NOTIFY_NTFY_TOPIC=from-env", // string, beats file
		"BACKPACK_NETWORK_PREFER_HOTSPOT=on",
		"BACKPACK_POWER_BATTERY_FLOOR=30",             // int
		"BACKPACK_THERMAL_CPU_SLEEP_C=",               // optional float: unset
		"BACKPACK_NETWORK_RECOVERY_MIN_INTERVAL=240s", // multi-underscore key
	}
	cfg, err := Load(LoadOptions{Path: p, Environ: env})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Session.Poll != 5*time.Second {
		t.Errorf("poll = %s", cfg.Session.Poll)
	}
	if cfg.Notify.NtfyTopic != "from-env" {
		t.Errorf("topic = %q", cfg.Notify.NtfyTopic)
	}
	if cfg.Network.PreferHotspot != HotspotOn {
		t.Errorf("prefer_hotspot = %q, want on", cfg.Network.PreferHotspot)
	}
	if cfg.Power.BatteryFloor != 30 {
		t.Errorf("battery_floor = %d", cfg.Power.BatteryFloor)
	}
	if cfg.Thermal.CPUSleepC != nil {
		t.Errorf("cpu_sleep_c = %v, want nil", *cfg.Thermal.CPUSleepC)
	}
	if cfg.Network.RecoveryMinInterval != 240*time.Second {
		t.Errorf("recovery_min_interval = %s", cfg.Network.RecoveryMinInterval)
	}

	cfg, err = Load(LoadOptions{Environ: []string{"BACKPACK_THERMAL_CPU_SLEEP_C=95"}})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Thermal.CPUSleepC == nil || *cfg.Thermal.CPUSleepC != 95 {
		t.Errorf("cpu_sleep_c = %v", cfg.Thermal.CPUSleepC)
	}
}

func TestEnvErrors(t *testing.T) {
	for _, env := range []string{
		"BACKPACK_THERMAL_BOGUS=1",
		"BACKPACK_SESSION_POLL=soon",
		"BACKPACK_NETWORK_WATCHDOG=maybe",
		"BACKPACK_POWER_BATTERY_FLOOR=lots",
		"BACKPACK_NETWORK_PREFER_HOTSPOT=sometimes",
		"BACKPACK_LOG_MAX_MB=0",
	} {
		if _, err := Load(LoadOptions{Environ: []string{env}}); err == nil {
			t.Errorf("%s: expected error", env)
		}
	}
}

func TestValidate(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	cases := map[string]func(*Config){
		"poll too small":        func(c *Config) { c.Session.Poll = 100 * time.Millisecond },
		"warning >= cap":        func(c *Config) { c.Session.MaxDurationWarning = c.Session.MaxDuration },
		"zero polls":            func(c *Config) { c.Thermal.CriticalPolls = 0 },
		"zero grace":            func(c *Config) { c.Thermal.SeriousGrace = 0 },
		"cpu_sleep_c too low":   func(c *Config) { c.Thermal.CPUSleepC = f(20) },
		"battery floor 100":     func(c *Config) { c.Power.BatteryFloor = 100 },
		"empty service":         func(c *Config) { c.Network.Service = " " },
		"bad ntfy server":       func(c *Config) { c.Notify.NtfyServer = "ntfy.sh" },
		"bad topic":             func(c *Config) { c.Notify.NtfyTopic = "has space" },
		"bad heartbeat prio":    func(c *Config) { c.Notify.HeartbeatPriority = 6 },
		"negative heartbeat":    func(c *Config) { c.Notify.Heartbeat = -time.Second },
		"zero recovery_after":   func(c *Config) { c.Network.RecoveryAfter = 0 },
		"zero recovery_min_int": func(c *Config) { c.Network.RecoveryMinInterval = 0 },
		"negative nudge after":  func(c *Config) { c.Network.WiFiNudgeAfter = -time.Second },
		"bad push":              func(c *Config) { c.Notify.Push = "some" },
		"negative outage_min":   func(c *Config) { c.Notify.OutageMin = -time.Second },
		"zero nudge every":      func(c *Config) { c.Network.WiFiNudgeEvery = 0 },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := Default()
			mutate(&cfg)
			if err := cfg.Validate(); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
	ok := Default()
	ok.Power.BatteryFloor = 0     // disables the floor
	ok.Network.WiFiNudgeAfter = 0 // disables the Wi-Fi nudge
	ok.Notify.Heartbeat = 0       // disables heartbeat
	ok.Thermal.CPUSleepC = f(95)
	if err := ok.Validate(); err != nil {
		t.Fatalf("valid edge values rejected: %v", err)
	}
}

func TestWriteTOMLRoundTrip(t *testing.T) {
	cfg := Default()
	c := 92.0
	cfg.Thermal.CPUSleepC = &c
	cfg.Notify.NtfyTopic = "round-trip"
	var buf bytes.Buffer
	if err := cfg.WriteTOML(&buf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), `poll = "15s"`) {
		t.Errorf("durations should encode as strings:\n%s", buf.String())
	}
	got, err := Parse(buf.Bytes())
	if err != nil {
		t.Fatalf("parse encoded config: %v\n%s", err, buf.String())
	}
	if got.Session != cfg.Session || got.Network != cfg.Network || got.Notify != cfg.Notify || got.Power != cfg.Power {
		t.Errorf("round trip mismatch:\n got %+v\nwant %+v", got, cfg)
	}
	if got.Thermal.CPUSleepC == nil || *got.Thermal.CPUSleepC != 92 {
		t.Errorf("cpu_sleep_c = %v", got.Thermal.CPUSleepC)
	}

	// Defaults (nil cpu_sleep_c) also round-trip.
	buf.Reset()
	if err := Default().WriteTOML(&buf); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "cpu_sleep_c") {
		t.Errorf("nil cpu_sleep_c should be omitted:\n%s", buf.String())
	}
	if _, err := Parse(buf.Bytes()); err != nil {
		t.Fatalf("parse encoded defaults: %v", err)
	}
}

func TestResolvePath(t *testing.T) {
	u := paths.ForHome("/Users/x")
	if p, explicit := ResolvePath("/flag.toml", []string{"BACKPACK_CONFIG=/env.toml"}, u); p != "/flag.toml" || !explicit {
		t.Errorf("flag: %q %v", p, explicit)
	}
	if p, explicit := ResolvePath("", []string{"BACKPACK_CONFIG=/env.toml"}, u); p != "/env.toml" || !explicit {
		t.Errorf("env: %q %v", p, explicit)
	}
	if p, explicit := ResolvePath("", nil, u); p != u.ConfigFile || explicit {
		t.Errorf("default: %q %v", p, explicit)
	}
}

func TestPreferHotspotParses(t *testing.T) {
	cases := []struct {
		toml string
		want HotspotPref
	}{
		{`prefer_hotspot = "auto"`, HotspotAuto},
		{`prefer_hotspot = "on"`, HotspotOn},
		{`prefer_hotspot = "OFF"`, HotspotOff},
		{"", HotspotAuto},
	}
	for _, c := range cases {
		cfg, err := Parse([]byte("[network]\n" + c.toml + "\n"))
		if err != nil {
			t.Errorf("%q: %v", c.toml, err)
			continue
		}
		if cfg.Network.PreferHotspot != c.want {
			t.Errorf("%q: got %q, want %q", c.toml, cfg.Network.PreferHotspot, c.want)
		}
	}
	for _, bad := range []string{`prefer_hotspot = "maybe"`, `prefer_hotspot = "true"`, "prefer_hotspot = true", "prefer_hotspot = 3"} {
		if _, err := Parse([]byte("[network]\n" + bad + "\n")); err == nil {
			t.Errorf("%q: expected an error", bad)
		}
	}
}

func TestPreferHotspotWritesString(t *testing.T) {
	var b bytes.Buffer
	if err := Default().WriteTOML(&b); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), `prefer_hotspot = "auto"`) {
		t.Errorf("WriteTOML:\n%s", b.String())
	}
	if !strings.Contains(b.String(), "[log]") || !strings.Contains(b.String(), "max_mb = 50") {
		t.Errorf("WriteTOML has no [log] section:\n%s", b.String())
	}
	// Round trip.
	if _, err := Parse(b.Bytes()); err != nil {
		t.Fatal(err)
	}
}

func TestLogSection(t *testing.T) {
	p := writeFile(t, "[log]\nmax_mb = 20\nmax_age = \"48h\"\n")
	cfg, err := Load(LoadOptions{Path: p})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Log.MaxMB != 20 || cfg.Log.MaxAge != 48*time.Hour {
		t.Errorf("log = %+v", cfg.Log)
	}
	cfg, err = Load(LoadOptions{Path: p, Environ: []string{"BACKPACK_LOG_MAX_MB=5", "BACKPACK_LOG_MAX_AGE=24h"}})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Log.MaxMB != 5 || cfg.Log.MaxAge != 24*time.Hour {
		t.Errorf("env log = %+v", cfg.Log)
	}
	if _, err := Parse([]byte("[log]\nmax_age = \"1m\"\n")); err == nil {
		t.Error("max_age 1m: expected an error")
	}
}

func TestSetString(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{
			name: "replace in section, keep comments",
			in:   "# mine\n[notify]\n# topic below\nntfy_topic = \"\"\nmacos = true\n[session]\nntfy_topic = \"x\"\n",
			want: "# mine\n[notify]\n# topic below\nntfy_topic = \"abc\"\nmacos = true\n[session]\nntfy_topic = \"x\"\n",
		},
		{
			name: "add after header",
			in:   "[notify]\nmacos = true\n",
			want: "[notify]\nntfy_topic = \"abc\"\nmacos = true\n",
		},
		{
			name: "append section",
			in:   "[session]\npoll = \"15s\"",
			want: "[session]\npoll = \"15s\"\n\n[notify]\nntfy_topic = \"abc\"\n",
		},
		{name: "empty file", in: "", want: "[notify]\nntfy_topic = \"abc\"\n"},
	}
	for _, c := range cases {
		got := string(SetString([]byte(c.in), "notify", "ntfy_topic", "abc"))
		if got != c.want {
			t.Errorf("%s:\ngot  %q\nwant %q", c.name, got, c.want)
		}
		var tree map[string]any
		if _, err := toml.Decode(got, &tree); err != nil {
			t.Errorf("%s: result is not TOML: %v", c.name, err)
		}
	}
}
