package cli

import (
	"errors"
	"flag"
	"fmt"
	"strings"
	"time"

	"github.com/ilias-t/bckp/internal/config"
	"github.com/ilias-t/bckp/internal/contracts"
	"github.com/ilias-t/bckp/internal/humanize"
)

// Session options (docs/DESIGN.md "Session options").
//
// sessionOptions is the one table that declares every session option. It
// drives, with no per-option code elsewhere:
//
//   - flag parsing (registerOptionFlags; the duration is the DUR argument),
//   - the TTY prompt for each option not given by a flag, in table order
//     (askOptions),
//   - the one-line summaries: after the prompts, the non-TTY detach line,
//     and `status` (describe/optionsLine),
//   - the detached child's argv (childArgs) and the engine's
//     Snapshot.Options, which carry each value's source to `status`.
//
// Safety rails and thresholds are config-only and never options.

// optFlag is one command-line flag of an option. A flag with a fixed
// value is boolean (--watchdog sets "on"); one without takes the value as
// its argument (--hotspot=auto).
type optFlag struct {
	name  string
	value string
	usage string
}

// sessionOption declares one session option.
type sessionOption struct {
	// name is the contracts.Option* key used in status and the snapshot.
	name string
	// arg: the value is the positional DUR argument, not a flag.
	arg   bool
	flags []optFlag
	// def is the value when the option is not given and not asked, and
	// what Enter picks at the prompt.
	def func(cfg config.Config) string
	// prompt is the question for the current default.
	prompt func(def string, cfg config.Config) string
	// parse maps a flag value, the DUR argument or a prompt answer to the
	// canonical value. It is never called with "".
	parse func(s string) (string, error)
	// hotspotOnly: only asked when the hotspot's network service exists.
	hotspotOnly bool
	// describe renders a value for the one-line summaries
	// ("hotspot auto"). The duration is described by the session line
	// itself, so it has none.
	describe func(v string) string
}

var sessionOptions = []sessionOption{
	{
		name: contracts.OptionDuration,
		arg:  true,
		def:  func(config.Config) string { return "forever" },
		prompt: func(def string, cfg config.Config) string {
			return fmt.Sprintf("How long? [%s] (%s cap) / 90 / 2h:", def, humanize.Duration(cfg.Session.MaxDuration))
		},
		parse: func(s string) (string, error) {
			d, forever, err := parseSessionDuration(s)
			if err != nil {
				return "", err
			}
			if forever {
				return "forever", nil
			}
			return canonicalDuration(d), nil
		},
	},
	{
		name:        contracts.OptionHotspot,
		hotspotOnly: true,
		flags: []optFlag{
			{name: "hotspot", usage: "prefer the iPhone hotspot over Wi-Fi: auto (once the phone is connected), on or off"},
		},
		def: func(cfg config.Config) string { return string(cfg.Network.PreferHotspot) },
		prompt: func(def string, cfg config.Config) string {
			choices := map[string]string{"auto": "a (auto, when plugged in)", "on": "y", "off": "n"}
			switch def {
			case "auto":
				choices["auto"] = "[A]uto (when plugged in)"
			case "on":
				choices["on"] = "[Y]es"
			case "off":
				choices["off"] = "[N]o"
			}
			return fmt.Sprintf("Prefer iPhone hotspot over Wi-Fi? %s / %s / %s:", choices["auto"], choices["on"], choices["off"])
		},
		parse: func(s string) (string, error) {
			switch strings.ToLower(strings.TrimSpace(s)) {
			case "a", "auto":
				return "auto", nil
			case "y", "yes", "on", "true":
				return "on", nil
			case "n", "no", "off", "false":
				return "off", nil
			}
			return "", fmt.Errorf("hotspot: want auto, on or off (a/y/n), got %q", s)
		},
		describe: func(v string) string { return "hotspot " + v },
	},
	{
		name: contracts.OptionWatchdog,
		flags: []optFlag{
			{name: "watchdog", value: "on", usage: "watch the network: outage alerts, hotspot link recovery"},
			{name: "no-watchdog", value: "off", usage: "don't watch the network"},
		},
		def: func(cfg config.Config) string { return humanize.OnOff(cfg.Network.Watchdog) },
		prompt: func(def string, cfg config.Config) string {
			yn := "[Y/n]"
			if def == "off" {
				yn = "[y/N]"
			}
			return "Watch the network (outage alerts, hotspot link recovery)? " + yn + ":"
		},
		parse: func(s string) (string, error) {
			switch strings.ToLower(strings.TrimSpace(s)) {
			case "y", "yes", "on", "true":
				return "on", nil
			case "n", "no", "off", "false":
				return "off", nil
			}
			return "", fmt.Errorf("watchdog: want y or n, got %q", s)
		},
		describe: func(v string) string { return "watchdog " + v },
	},
}

// canonicalDuration renders a session length so that parseSessionDuration
// reads it back unchanged: "90m" style minutes, or Go's form when there
// are seconds.
func canonicalDuration(d time.Duration) string {
	if d%time.Minute == 0 {
		return humanize.Duration(d)
	}
	return d.String()
}

// choice is one resolved option.
type choice struct {
	value  string
	source string // contracts.Source*
	flag   string // the flag that set it, for conflict messages
}

// choices maps option names to resolved values.
type choices map[string]choice

// set records a flag's value, reporting a conflict with an earlier flag
// that set the same option differently.
func (c choices) set(option, value, flag string) error {
	if prev, ok := c[option]; ok && prev.value != value {
		return fmt.Errorf("%s and %s conflict", prev.flag, flag)
	}
	if _, ok := c[option]; !ok {
		c[option] = choice{value: value, source: contracts.SourceFlag, flag: flag}
	}
	return nil
}

// registerOptionFlags adds every option's flags to fs. Values land in got; the first conflict is kept in *errp.
func registerOptionFlags(fs *flag.FlagSet, got choices, errp *error) {
	record := func(option, value, flag string) {
		if err := got.set(option, value, flag); err != nil && *errp == nil {
			*errp = err
		}
	}
	for _, o := range sessionOptions {
		for _, f := range o.flags {
			name := "--" + f.name
			if f.value != "" {
				fs.BoolFunc(f.name, f.usage, func(s string) error {
					if s != "true" {
						return fmt.Errorf("takes no value")
					}
					record(o.name, f.value, name)
					return nil
				})
				continue
			}
			fs.Func(f.name, f.usage, func(s string) error {
				v, err := o.parse(s)
				if err != nil {
					return err
				}
				record(o.name, v, name+"="+v)
				return nil
			})
		}
	}
}

// setArg records the positional DUR argument.
func (c choices) setArg(s string) error {
	for _, o := range sessionOptions {
		if !o.arg {
			continue
		}
		v, err := o.parse(s)
		if err != nil {
			return err
		}
		return c.set(o.name, v, s)
	}
	return errors.New("no positional option")
}

// fillDefaults sets every option not yet chosen to its default.
func (c choices) fillDefaults(cfg config.Config) {
	for _, o := range sessionOptions {
		if _, ok := c[o.name]; !ok {
			c[o.name] = choice{value: o.def(cfg), source: contracts.SourceDefault}
		}
	}
}

// errPromptAborted: stdin ended at a prompt; nothing was started.
var errPromptAborted = errors.New("no answer (end of input); nothing started")

// maxPromptTries bounds re-asking after an invalid answer.
const maxPromptTries = 3

// askOptions prompts, in table order, for every option not given by a
// flag. Enter takes the shown default. Without the hotspot's network
// service (no iPhone ever connected over USB), the hotspot question is
// skipped and its default kept.
func askOptions(p *prompter, c choices, cfg config.Config, haveHotspot bool) error {
	for _, o := range sessionOptions {
		if _, ok := c[o.name]; ok || (o.hotspotOnly && !haveHotspot) {
			continue
		}
		def := o.def(cfg)
		v, err := p.choose(o.prompt(def, cfg), def, o.parse)
		if err != nil {
			return err
		}
		src := contracts.SourcePrompt
		c[o.name] = choice{value: v, source: src}
	}
	return nil
}

// list returns the options in table order for the engine snapshot.
func (c choices) list() []contracts.SessionOption {
	var out []contracts.SessionOption
	for _, o := range sessionOptions {
		if ch, ok := c[o.name]; ok {
			out = append(out, contracts.SessionOption{Name: o.name, Value: ch.value, Source: ch.source})
		}
	}
	return out
}

// sourcesArg encodes the sources for the detached child's --sources.
func (c choices) sourcesArg() string {
	var parts []string
	for _, o := range sessionOptions {
		if ch, ok := c[o.name]; ok {
			parts = append(parts, o.name+"="+ch.source)
		}
	}
	return strings.Join(parts, ",")
}

// applySources overrides sources from a --sources value (the child keeps
// what the parent resolved, e.g. "prompt").
func (c choices) applySources(s string) error {
	if s == "" {
		return nil
	}
	for _, kv := range strings.Split(s, ",") {
		k, v, ok := strings.Cut(kv, "=")
		ch, known := c[k]
		switch {
		case !ok || !known:
			return fmt.Errorf("--sources: bad entry %q", kv)
		case v != contracts.SourceFlag && v != contracts.SourcePrompt && v != contracts.SourceDefault:
			return fmt.Errorf("--sources: bad source %q", v)
		}
		ch.source = v
		c[k] = ch
	}
	return nil
}

// childFlags renders the options as the detached child's arguments: one
// flag per option carrying its canonical value, then DUR.
func (c choices) childFlags() []string {
	var args []string
	var dur string
	for _, o := range sessionOptions {
		ch, ok := c[o.name]
		if !ok {
			continue
		}
		if o.arg {
			dur = ch.value
			continue
		}
		args = append(args, flagFor(o, ch.value))
	}
	if dur != "" {
		args = append(args, dur)
	}
	return args
}

// flagFor picks the flag that expresses value: a value flag if the option
// has one, else the boolean flag with that fixed value.
func flagFor(o sessionOption, value string) string {
	for _, f := range o.flags {
		if f.value == "" {
			return "--" + f.name + "=" + value
		}
	}
	for _, f := range o.flags {
		if f.value == value {
			return "--" + f.name
		}
	}
	panic(fmt.Sprintf("cli: no flag for %s=%s", o.name, value))
}

// engineOptions maps the resolved options onto the engine options and the
// config the engine runs with (Network.PreferHotspot, Network.Watchdog).
func (c choices) engineOptions(cfg *config.Config) (EngineOptions, error) {
	var opts EngineOptions
	if dur := c[contracts.OptionDuration].value; dur != "" && dur != "forever" {
		d, _, err := parseSessionDuration(dur)
		if err != nil {
			return opts, err
		}
		opts.Duration = d
	}
	if v := c[contracts.OptionHotspot].value; v != "" {
		h, err := config.ParseHotspotPref(v)
		if err != nil {
			return opts, err
		}
		cfg.Network.PreferHotspot = h
	}
	if v := c[contracts.OptionWatchdog].value; v != "" {
		cfg.Network.Watchdog = v == "on"
	}
	opts.Choices = c.list()
	return opts, nil
}

// describeOptions renders the non-duration options of a snapshot (or of
// resolved choices) as " · hotspot auto · watchdog on"; "" when there are
// none.
func describeOptions(opts []contracts.SessionOption) string {
	var b strings.Builder
	for _, so := range opts {
		o, ok := optionFor(so.Name)
		if !ok || o.describe == nil {
			continue
		}
		b.WriteString(" · " + o.describe(so.Value))
	}
	return b.String()
}

func optionFor(name string) (sessionOption, bool) {
	for _, o := range sessionOptions {
		if o.name == name {
			return o, true
		}
	}
	return sessionOption{}, false
}

// choicesSummary is the line printed after the prompts:
// "forever (12h cap) · hotspot auto · watchdog on".
func choicesSummary(c choices, cfg config.Config) string {
	dur := c[contracts.OptionDuration].value
	head := dur
	if dur == "forever" {
		head = fmt.Sprintf("forever (%s cap)", humanize.Duration(cfg.Session.MaxDuration))
	} else if d, _, err := parseSessionDuration(dur); err == nil && d > cfg.Session.MaxDuration {
		head = fmt.Sprintf("%s (capped at %s)", dur, humanize.Duration(cfg.Session.MaxDuration))
	}
	return head + describeOptions(c.list())
}

// optionsStatus renders the status row: every option with its source,
// "duration forever (default) · hotspot auto (prompt) · watchdog on (flag)".
func optionsStatus(opts []contracts.SessionOption) string {
	parts := make([]string, 0, len(opts))
	for _, so := range opts {
		parts = append(parts, fmt.Sprintf("%s %s (%s)", so.Name, so.Value, so.Source))
	}
	return strings.Join(parts, " · ")
}
