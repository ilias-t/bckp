// Package config loads backpack's TOML configuration
// (~/.config/backpack/config.toml), applies defaults, BACKPACK_*
// environment overrides, and validates the result.
//
// Precedence, lowest to highest: Default(), the TOML file, environment.
//
// Environment overrides are named BACKPACK_<SECTION>_<KEY>, upper-cased
// from the TOML names, e.g. BACKPACK_SESSION_POLL=10s,
// BACKPACK_NOTIFY_NTFY_TOPIC=my-topic, BACKPACK_THERMAL_CPU_SLEEP_C=95.
// Values use the TOML value syntax without quotes: durations as Go
// durations ("90s", "12h"), booleans as true/false/1/0, numbers as
// decimals. An empty BACKPACK_THERMAL_CPU_SLEEP_C unsets the optional
// threshold. Unknown keys in a known section are an error; variables
// outside the known sections (e.g. BACKPACK_CONFIG, BACKPACK_E2E) are
// ignored.
package config

import (
	"bytes"
	"encoding"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/ilias-t/bckp/internal/paths"
	"github.com/ilias-t/bckp/internal/proto"
)

// EnvConfigPath names the environment variable that overrides the config
// file location (below an explicit --config flag).
const EnvConfigPath = "BACKPACK_CONFIG"

// EnvPrefix prefixes all override variables.
const EnvPrefix = "BACKPACK_"

// Config is the full configuration.
type Config struct {
	Session Session `toml:"session"`
	Thermal Thermal `toml:"thermal"`
	Power   Power   `toml:"power"`
	Network Network `toml:"network"`
	Notify  Notify  `toml:"notify"`
	Log     Log     `toml:"log"`
}

// Log configures the engine's rotating event log (internal/eventlog). The
// root daemon ignores it and always uses the eventlog defaults.
type Log struct {
	// MaxMB caps the log's active plus rotated files, in MiB.
	MaxMB int `toml:"max_mb"`
	// MaxAge deletes rotated files last written longer ago.
	MaxAge time.Duration `toml:"max_age"`
}

// HotspotPref is network.prefer_hotspot: whether the session moves the
// hotspot service (network.service) ahead of Wi-Fi.
type HotspotPref string

const (
	// HotspotAuto prefers the hotspot only once it is actually connected
	// (interface present and gateway answering); the engine checks every
	// poll, so plugging the phone in mid-session engages it.
	HotspotAuto HotspotPref = "auto"
	// HotspotOn reorders the services at session start.
	HotspotOn HotspotPref = "on"
	// HotspotOff never reorders the services.
	HotspotOff HotspotPref = "off"
)

// ParseHotspotPref parses auto, on or off (any case).
func ParseHotspotPref(s string) (HotspotPref, error) {
	switch p := HotspotPref(strings.ToLower(strings.TrimSpace(s))); p {
	case HotspotAuto, HotspotOn, HotspotOff:
		return p, nil
	}
	return "", fmt.Errorf("prefer_hotspot must be \"auto\", \"on\" or \"off\" (got %q)", s)
}

// UnmarshalText parses the TOML value and BACKPACK_NETWORK_PREFER_HOTSPOT.
func (h *HotspotPref) UnmarshalText(b []byte) error {
	p, err := ParseHotspotPref(string(b))
	if err != nil {
		return err
	}
	*h = p
	return nil
}

// MarshalText writes the preference as a TOML string.
func (h HotspotPref) MarshalText() ([]byte, error) { return []byte(h), nil }

// Session configures the session timer and polling.
type Session struct {
	// Poll is the provider poll interval for all state machines.
	Poll time.Duration `toml:"poll"`
	// MaxDuration caps every session, including forever; extend resets it.
	MaxDuration time.Duration `toml:"max_duration"`
	// MaxDurationWarning is how long before the cap the warning is sent.
	MaxDurationWarning time.Duration `toml:"max_duration_warning"`
}

// Thermal configures the thermal state machine. Counts are consecutive
// polls.
type Thermal struct {
	// FairPolls fair polls in a row enter FAIR.
	FairPolls int `toml:"fair_polls"`
	// FairCooldown is the minimum gap between FAIR notifications.
	FairCooldown time.Duration `toml:"fair_cooldown"`
	// HotRenotify is the repeat interval for HOT notifications.
	HotRenotify time.Duration `toml:"hot_renotify"`
	// SeriousSleepAfter of continuous HOT enters SLEEP_PENDING.
	SeriousSleepAfter time.Duration `toml:"serious_sleep_after"`
	// SeriousGrace is the (abortable) grace before a serious sleep.
	SeriousGrace time.Duration `toml:"serious_grace"`
	// CriticalPolls critical polls in a row enter SLEEP_PENDING.
	CriticalPolls int `toml:"critical_polls"`
	// CriticalGrace is the (non-abortable) grace before a critical sleep.
	CriticalGrace time.Duration `toml:"critical_grace"`
	// CooledPolls polls below serious reset the hot timer ("cooled down").
	CooledPolls int `toml:"cooled_polls"`
	// NominalPolls nominal polls send "back to nominal".
	NominalPolls int `toml:"nominal_polls"`
	// CPUSleepC, when set, triggers an abortable sleep (SeriousGrace) once
	// CPU °C reaches it. nil disables the °C trigger (default).
	CPUSleepC *float64 `toml:"cpu_sleep_c"`
	// BatteryWarnC, BatterySleepC and BatteryCriticalC are battery cell
	// °C thresholds, each reached for 2 polls in a row: a warning, an
	// abortable sleep (SeriousGrace), and a non-abortable sleep
	// (CriticalGrace). They guard the case the thermal state misses: a
	// closed bag heats the battery while the chip stays cool enough that
	// macOS reports nominal. Apple warns that ambient heat above 35°C
	// permanently damages battery capacity; the cell reads roughly 10°C
	// above ambient, so 45°C in the cell is about a 35°C room.
	BatteryWarnC     float64 `toml:"battery_warn_c"`
	BatterySleepC    float64 `toml:"battery_sleep_c"`
	BatteryCriticalC float64 `toml:"battery_critical_c"`
}

// Power configures the power rails.
type Power struct {
	// BatteryFloor ends the session (after notifying) when on battery at
	// or below this percent. 0 disables the floor.
	BatteryFloor int `toml:"battery_floor"`
	// NotifyOnBattery notifies when power switches from AC to battery.
	NotifyOnBattery bool `toml:"notify_on_battery"`
}

// Network configures the network watchdog and hotspot preference.
type Network struct {
	// Watchdog enables outage detection and service off/on recovery.
	Watchdog bool `toml:"watchdog"`
	// RecoveryAfter offline (with the hotspot interface present) before
	// toggling the service.
	RecoveryAfter time.Duration `toml:"recovery_after"`
	// RecoveryMinInterval is the minimum gap between toggles.
	RecoveryMinInterval time.Duration `toml:"recovery_min_interval"`
	// Service is the hotspot network service name.
	Service string `toml:"service"`
	// PreferHotspot is auto, on or off: whether Service is moved ahead of
	// Wi-Fi for the session. A session flag or prompt overrides it.
	PreferHotspot HotspotPref `toml:"prefer_hotspot"`
	// WiFiNudgeAfter offline, with the iPhone not on USB, before the
	// engine cycles Wi-Fi power (or turns it on) so macOS auto-joins the
	// iPhone hotspot over Wi-Fi. Only with prefer_hotspot = "on"; never
	// while online. 0 disables the nudge.
	WiFiNudgeAfter time.Duration `toml:"wifi_nudge_after"`
	// WiFiNudgeEvery is the minimum gap between Wi-Fi nudges.
	WiFiNudgeEvery time.Duration `toml:"wifi_nudge_every"`
	// Iface overrides the hotspot BSD interface (e.g. "en10"); "" means
	// resolve it from Service.
	Iface string `toml:"iface"`
	// Gateway overrides the hotspot gateway (e.g. "172.20.10.1"); ""
	// means discover it.
	Gateway string `toml:"gateway"`
	// WiFiIface overrides the Wi-Fi interface (e.g. "en0"); "" means
	// discover it.
	WiFiIface string `toml:"wifi_iface"`
}

// Push policies for Notify.Push.
const (
	// PushAttention pushes only notifications that need attention
	// (contracts.ClassAttention); info ones are logged (default).
	PushAttention = "attention"
	// PushAll pushes every notification, info included.
	PushAll = "all"
)

// Notify configures notifications.
type Notify struct {
	// Push is PushAttention or PushAll.
	Push string `toml:"push"`
	// OutageMin is how long an outage must last for its "back online"
	// message to need attention; shorter ones are info (logged only with
	// Push = PushAttention).
	OutageMin time.Duration `toml:"outage_min"`
	// NtfyServer is the ntfy base URL.
	NtfyServer string `toml:"ntfy_server"`
	// NtfyTopic is the ntfy topic; "" disables ntfy.
	NtfyTopic string `toml:"ntfy_topic"`
	// Heartbeat is the periodic status interval during a session; 0
	// (the default) disables it. It is info: pushed only with Push =
	// PushAll. The event log's once-a-minute sample records the same.
	Heartbeat time.Duration `toml:"heartbeat"`
	// HeartbeatPriority is the heartbeat's priority (1–5).
	HeartbeatPriority int `toml:"heartbeat_priority"`
	// MacOS posts macOS notifications (on by default, so alerts reach the
	// user without an ntfy topic).
	MacOS bool `toml:"macos"`
}

// Default returns the built-in defaults.
func Default() Config {
	return Config{
		Session: Session{
			Poll:               15 * time.Second,
			MaxDuration:        12 * time.Hour,
			MaxDurationWarning: 10 * time.Minute,
		},
		Thermal: Thermal{
			FairPolls:         2,
			FairCooldown:      30 * time.Minute,
			HotRenotify:       10 * time.Minute,
			SeriousSleepAfter: 3 * time.Minute,
			SeriousGrace:      30 * time.Second,
			CriticalPolls:     2,
			CriticalGrace:     15 * time.Second,
			CooledPolls:       8,
			NominalPolls:      3,
			CPUSleepC:         nil,
			BatteryWarnC:      40,
			BatterySleepC:     45,
			BatteryCriticalC:  50,
		},
		Power: Power{
			BatteryFloor:    25,
			NotifyOnBattery: true,
		},
		Network: Network{
			Watchdog:            true,
			RecoveryAfter:       90 * time.Second,
			RecoveryMinInterval: 180 * time.Second,
			Service:             "iPhone USB",
			PreferHotspot:       HotspotAuto,
			WiFiNudgeAfter:      60 * time.Second,
			WiFiNudgeEvery:      3 * time.Minute,
		},
		Notify: Notify{
			Push:              PushAttention,
			OutageMin:         5 * time.Minute,
			NtfyServer:        "https://ntfy.sh",
			NtfyTopic:         "",
			Heartbeat:         0,
			HeartbeatPriority: 2,
			MacOS:             true,
		},
		Log: Log{
			MaxMB:  50,
			MaxAge: 7 * 24 * time.Hour,
		},
	}
}

// LoadOptions controls Load.
type LoadOptions struct {
	// Path is the TOML file. "" skips the file (defaults + env only).
	Path string
	// MustExist makes a missing file an error. Set it when the path came
	// from --config or BACKPACK_CONFIG; leave it false for the default
	// path so a fresh install runs on defaults.
	MustExist bool
	// Environ is the environment as KEY=VALUE pairs (os.Environ()); nil
	// means no overrides.
	Environ []string
}

// ResolvePath picks the config path: flag, then BACKPACK_CONFIG from
// environ, then the default under u. explicit reports whether the path was
// chosen by the user (and so should be loaded with MustExist).
func ResolvePath(flag string, environ []string, u paths.User) (path string, explicit bool) {
	if flag != "" {
		return flag, true
	}
	if v, ok := LookupEnv(environ, EnvConfigPath); ok && v != "" {
		return v, true
	}
	return u.ConfigFile, false
}

// Load builds a Config from defaults, the file, and the environment, then
// validates it.
func Load(opts LoadOptions) (Config, error) {
	cfg := Default()
	if opts.Path != "" {
		data, err := os.ReadFile(opts.Path)
		switch {
		case err == nil:
			if err := decodeTOML(data, &cfg); err != nil {
				return Config{}, fmt.Errorf("config %s: %w", opts.Path, err)
			}
		case errors.Is(err, fs.ErrNotExist) && !opts.MustExist:
		default:
			return Config{}, fmt.Errorf("config: %w", err)
		}
	}
	if err := applyEnv(&cfg, opts.Environ); err != nil {
		return Config{}, err
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Parse decodes TOML over the defaults and validates (no environment).
func Parse(data []byte) (Config, error) {
	cfg := Default()
	if err := decodeTOML(data, &cfg); err != nil {
		return Config{}, err
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func decodeTOML(data []byte, cfg *Config) error {
	md, err := toml.Decode(string(data), cfg)
	if err != nil {
		return err
	}
	if und := md.Undecoded(); len(und) > 0 {
		keys := make([]string, len(und))
		for i, k := range und {
			keys[i] = k.String()
		}
		return fmt.Errorf("unknown keys: %s", strings.Join(keys, ", "))
	}
	return nil
}

// WriteTOML encodes c as TOML (used by `config init` and `config show`).
// A nil CPUSleepC is omitted.
func (c Config) WriteTOML(w io.Writer) error {
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(c); err != nil {
		return err
	}
	_, err := w.Write(buf.Bytes())
	return err
}

var ntfyTopicRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// ValidTopic reports whether s is a usable ntfy topic: 1-64 letters,
// digits, - or _.
func ValidTopic(s string) bool { return ntfyTopicRe.MatchString(s) }

// Validate reports every invalid field, joined.
func (c Config) Validate() error {
	var errs []error
	check := func(ok bool, format string, args ...any) {
		if !ok {
			errs = append(errs, fmt.Errorf(format, args...))
		}
	}

	s := c.Session
	check(s.Poll >= time.Second, "session.poll must be >= 1s (got %s)", s.Poll)
	check(s.MaxDuration > 0, "session.max_duration must be > 0 (got %s)", s.MaxDuration)
	check(s.MaxDurationWarning >= 0 && s.MaxDurationWarning < s.MaxDuration,
		"session.max_duration_warning must be >= 0 and < max_duration (got %s)", s.MaxDurationWarning)

	t := c.Thermal
	for name, n := range map[string]int{
		"fair_polls": t.FairPolls, "critical_polls": t.CriticalPolls,
		"cooled_polls": t.CooledPolls, "nominal_polls": t.NominalPolls,
	} {
		check(n >= 1, "thermal.%s must be >= 1 (got %d)", name, n)
	}
	for name, d := range map[string]time.Duration{
		"fair_cooldown": t.FairCooldown, "hot_renotify": t.HotRenotify,
		"serious_sleep_after": t.SeriousSleepAfter, "serious_grace": t.SeriousGrace,
		"critical_grace": t.CriticalGrace,
	} {
		check(d > 0, "thermal.%s must be > 0 (got %s)", name, d)
	}
	if t.CPUSleepC != nil {
		check(*t.CPUSleepC >= 40 && *t.CPUSleepC <= 120,
			"thermal.cpu_sleep_c must be 40..120 (got %g)", *t.CPUSleepC)
	}
	for name, v := range map[string]float64{
		"battery_warn_c": t.BatteryWarnC, "battery_sleep_c": t.BatterySleepC, "battery_critical_c": t.BatteryCriticalC,
	} {
		// The root helper sleeps the Mac at proto.BackstopBatteryTempC
		// whatever the config says, so higher values would never apply.
		check(v >= 30 && v <= proto.BackstopBatteryTempC, "thermal.%s must be 30..%g (got %g)", name, proto.BackstopBatteryTempC, v)
	}
	check(t.BatteryWarnC < t.BatterySleepC && t.BatterySleepC < t.BatteryCriticalC,
		"thermal battery thresholds must rise: battery_warn_c < battery_sleep_c < battery_critical_c (got %g, %g, %g)",
		t.BatteryWarnC, t.BatterySleepC, t.BatteryCriticalC)

	p := c.Power
	check(p.BatteryFloor >= 0 && p.BatteryFloor <= 99, "power.battery_floor must be 0..99 (got %d)", p.BatteryFloor)

	n := c.Network
	check(n.RecoveryAfter > 0, "network.recovery_after must be > 0 (got %s)", n.RecoveryAfter)
	check(n.RecoveryMinInterval > 0, "network.recovery_min_interval must be > 0 (got %s)", n.RecoveryMinInterval)
	check(strings.TrimSpace(n.Service) != "", "network.service must not be empty")
	check(n.PreferHotspot == HotspotAuto || n.PreferHotspot == HotspotOn || n.PreferHotspot == HotspotOff,
		"network.prefer_hotspot must be %q, %q or %q (got %q)", HotspotAuto, HotspotOn, HotspotOff, n.PreferHotspot)
	check(n.WiFiNudgeAfter >= 0, "network.wifi_nudge_after must be >= 0; 0 disables the nudge (got %s)", n.WiFiNudgeAfter)
	check(n.WiFiNudgeEvery > 0, "network.wifi_nudge_every must be > 0 (got %s); set wifi_nudge_after = 0 to disable", n.WiFiNudgeEvery)

	no := c.Notify
	if u, err := url.Parse(no.NtfyServer); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		errs = append(errs, fmt.Errorf("notify.ntfy_server must be an http(s) URL (got %q)", no.NtfyServer))
	}
	check(no.NtfyTopic == "" || ntfyTopicRe.MatchString(no.NtfyTopic),
		"notify.ntfy_topic must match [A-Za-z0-9_-]{1,64} (got %q)", no.NtfyTopic)
	check(no.Heartbeat >= 0, "notify.heartbeat must be >= 0 (got %s)", no.Heartbeat)
	check(no.Push == PushAttention || no.Push == PushAll, "notify.push must be %q or %q (got %q)", PushAttention, PushAll, no.Push)
	check(no.OutageMin >= 0, "notify.outage_min must be >= 0 (got %s)", no.OutageMin)
	check(no.HeartbeatPriority >= 1 && no.HeartbeatPriority <= 5,
		"notify.heartbeat_priority must be 1..5 (got %d)", no.HeartbeatPriority)

	l := c.Log
	check(l.MaxMB >= 1, "log.max_mb must be >= 1 (got %d)", l.MaxMB)
	check(l.MaxAge >= time.Hour, "log.max_age must be >= 1h (got %s)", l.MaxAge)

	sort.Slice(errs, func(i, j int) bool { return errs[i].Error() < errs[j].Error() })
	return errors.Join(errs...)
}

// LookupEnv returns the last value of key in environ (KEY=VALUE pairs).
func LookupEnv(environ []string, key string) (string, bool) {
	for i := len(environ) - 1; i >= 0; i-- {
		k, v, ok := strings.Cut(environ[i], "=")
		if ok && k == key {
			return v, true
		}
	}
	return "", false
}

var durationType = reflect.TypeOf(time.Duration(0))

// applyEnv applies BACKPACK_<SECTION>_<KEY> overrides.
func applyEnv(cfg *Config, environ []string) error {
	root := reflect.ValueOf(cfg).Elem()
	sections := map[string]reflect.Value{}
	for i := 0; i < root.NumField(); i++ {
		sections[strings.ToUpper(root.Type().Field(i).Tag.Get("toml"))] = root.Field(i)
	}

	var errs []error
	for _, kv := range environ {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || !strings.HasPrefix(k, EnvPrefix) {
			continue
		}
		rest := strings.TrimPrefix(k, EnvPrefix)
		secName, key, ok := strings.Cut(rest, "_")
		if !ok {
			continue
		}
		sec, known := sections[secName]
		if !known {
			continue
		}
		field, found := fieldByTOMLName(sec, strings.ToLower(key))
		if !found {
			errs = append(errs, fmt.Errorf("%s: unknown config key %s.%s", k, strings.ToLower(secName), strings.ToLower(key)))
			continue
		}
		if err := setFromString(field, v); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", k, err))
		}
	}
	return errors.Join(errs...)
}

func fieldByTOMLName(sec reflect.Value, name string) (reflect.Value, bool) {
	for i := 0; i < sec.NumField(); i++ {
		if sec.Type().Field(i).Tag.Get("toml") == name {
			return sec.Field(i), true
		}
	}
	return reflect.Value{}, false
}

func setFromString(f reflect.Value, s string) error {
	s = strings.TrimSpace(s)
	if tu, ok := f.Addr().Interface().(encoding.TextUnmarshaler); ok {
		return tu.UnmarshalText([]byte(s))
	}
	switch {
	case f.Type() == durationType:
		d, err := time.ParseDuration(s)
		if err != nil {
			return err
		}
		f.SetInt(int64(d))
	case f.Kind() == reflect.String:
		f.SetString(s)
	case f.Kind() == reflect.Bool:
		b, err := strconv.ParseBool(s)
		if err != nil {
			return err
		}
		f.SetBool(b)
	case f.Kind() == reflect.Int:
		n, err := strconv.Atoi(s)
		if err != nil {
			return err
		}
		f.SetInt(int64(n))
	case f.Kind() == reflect.Float64:
		x, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return err
		}
		f.SetFloat(x)
	case f.Kind() == reflect.Pointer && f.Type().Elem().Kind() == reflect.Float64:
		if s == "" {
			f.Set(reflect.Zero(f.Type()))
			return nil
		}
		x, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return err
		}
		f.Set(reflect.ValueOf(&x))
	default:
		return fmt.Errorf("unsupported field type %s", f.Type())
	}
	return nil
}
