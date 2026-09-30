// Package simulate runs the real engine against scripted fake providers
// and a fake clock, and prints a timeline of state changes, notifications
// and sleep-guard calls. Nothing is sent, nothing sleeps, and no system
// setting changes. It backs `bckp simulate`, the way to QA thresholds.
// The script language is described by Grammar.
package simulate

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/ilias-t/bckp/internal/config"
	"github.com/ilias-t/bckp/internal/contracts"
)

// Grammar describes the script language, for `bckp simulate --help`.
const Grammar = `A script is a whitespace-separated list of tokens. Every input dimension
(thermal, CPU °C, power, network) is sticky: it keeps its last value
until a token changes it. Polls happen every session.poll (default 15s);
poll 1 is at session start.

Poll tokens set one dimension and consume one poll. "xN" right after a
poll token makes it N polls in total ("serious x40" is 40 serious polls,
i.e. 10 minutes at the default poll):

  nominal | fair | serious | critical   thermal state for the poll
  unknown                               thermal state unknown (a failed reading)
  fail                                  thermal read error
  online                                network up (via the hotspot when plugged in, else Wi-Fi)
  offline                               no default route (hotspot interface present when plugged in)
  offline:noiface                       no default route, hotspot interface missing
  wifi:hotspot                          phone not on USB; online via its hotspot over Wi-Fi
                                        (gateway 172.20.10.1)
  wifi:other                            phone not on USB; online via another Wi-Fi network
  wifi:none                             phone not on USB; no default route (Wi-Fi joined nothing)
  net:fail                              network probe error
  poll                                  one poll with nothing changed
  wait:DUR                              polls covering DUR (e.g. wait:30m), nothing changed

Setting tokens change a sticky dimension from the next poll on and
consume no poll:

  ac:on | ac:off                        external power connected / on battery
  batt:N                                battery percent (0-100)
  lid:open | lid:closed                 clamshell state (informational)
  cpu:N | cpu:none                      CPU °C (for thermal.cpu_sleep_c)
  btemp:N | btemp:none                  battery °C (default 30; for thermal.battery_*_c)
  unplug | plug                         iPhone unplugged (no hotspot interface; online
                                        means Wi-Fi) / plugged back in (the default).
                                        The wifi:* tokens unplug too.

While the simulated Wi-Fi is off (a Wi-Fi nudge between its off and on
phases), an unplugged phone means no default route whatever the token
says.

Command tokens run right after the preceding poll, before the next one:

  stop | stop:sleep | stop:nosleep      bckp stop (sleep unset / --sleep / --no-sleep)
  extend:N | extend:forever             bckp extend N (minutes) / forever
  lease:lost                            the daemon lease drops

Session tokens must come before the first poll token:

  for:DUR                               session duration (default: forever)
  forever                               run until stopped (the default)
  hotspot | hotspot:auto|on|off         --hotspot=on / --hotspot=X (default: the
                                        config's network.prefer_hotspot)
  watchdog:on|off                       --watchdog / --no-watchdog

Example: "nominal x4 fair x3 serious x45 nominal x5",
"batt:30 ac:off offline x10 online", or, for the auto hotspot preference,
"hotspot:auto unplug online x4 plug online x4 unplug online x4". The
Wi-Fi nudge (hotspot "on" only): "hotspot:on wifi:other x4 wifi:none x8
wifi:hotspot x4".

When the script runs out while a sleep is pending, the simulation keeps
polling (repeating the last inputs) until the grace period ends;
otherwise it ends the session with "bckp stop".
`

// thermalInput is the thermal part of one poll.
type thermalInput struct {
	state contracts.ThermalState
	fail  bool
	cpu   *float64
	battC *float64
}

// netInput is the network part of one poll.
type netInput struct {
	kind string // "online", "offline", "offline:noiface", "wifi:hotspot", "wifi:other", "wifi:none", "net:fail"
	// unplugged: the iPhone is not connected (no hotspot interface).
	unplugged bool
}

// pollInput is everything the fake providers return for one poll.
type pollInput struct {
	thermal thermalInput
	power   contracts.PowerReading
	net     netInput
}

type command struct {
	token   string
	stop    bool
	sleep   *bool
	extend  bool
	minutes int
	forever bool
	lost    bool
}

// plan is a parsed script.
type plan struct {
	polls []pollInput
	// cmds[i] run after poll i (0-based).
	cmds     map[int][]command
	duration time.Duration
	forever  bool
	// hotspot is the hotspot preference ("" = the config's).
	hotspot config.HotspotPref
	// watchdog overrides network.watchdog when set.
	watchdog *bool
	// btemp: the script sets battery °C, so poll lines show it.
	btemp bool
}

func parse(script string, pollEvery time.Duration) (*plan, error) {
	p := &plan{cmds: map[int][]command{}, forever: true}
	calmBattery := 30.0
	cur := pollInput{
		thermal: thermalInput{state: contracts.ThermalNominal, battC: &calmBattery},
		power:   contracts.PowerReading{OnAC: true, Percent: 100, LidClosed: true, SleepDisabled: true},
		net:     netInput{kind: "online"},
	}
	lastWasPoll := false
	pendingSettings := false
	toks := strings.Fields(script)
	if len(toks) == 0 {
		return nil, fmt.Errorf("empty script")
	}
	for i, tok := range toks {
		bad := func(format string, args ...any) error {
			return fmt.Errorf("token %d %q: %s", i+1, tok, fmt.Sprintf(format, args...))
		}
		lower := strings.ToLower(tok)
		name, arg, hasArg := strings.Cut(lower, ":")
		addPoll := func() {
			p.polls = append(p.polls, cur)
			lastWasPoll, pendingSettings = true, false
		}

		// xN repeat.
		if len(lower) > 1 && lower[0] == 'x' && isDigits(lower[1:]) {
			n, _ := strconv.Atoi(lower[1:])
			if !lastWasPoll {
				return nil, bad("xN must follow a poll token")
			}
			if n < 1 {
				return nil, bad("repeat count must be >= 1")
			}
			last := p.polls[len(p.polls)-1]
			for range n - 1 {
				p.polls = append(p.polls, last)
			}
			lastWasPoll = false // "serious x3 x2" is an error
			continue
		}
		lastWasPoll = false

		switch {
		case !hasArg && (name == "nominal" || name == "fair" || name == "serious" || name == "critical" || name == "unknown"):
			st, _ := contracts.ParseThermalState(name)
			cur.thermal.state = st
			addPoll()
		case !hasArg && name == "fail":
			// One-off: the thermal state stays what it was.
			cur.thermal.fail = true
			addPoll()
			cur.thermal.fail = false
		case lower == "online" || lower == "offline" || lower == "offline:noiface":
			cur.net.kind = lower
			addPoll()
		case lower == "wifi:hotspot" || lower == "wifi:other" || lower == "wifi:none":
			cur.net.kind, cur.net.unplugged = lower, true
			addPoll()
		case lower == "net:fail":
			// One-off: the network state stays what it was.
			prev := cur.net.kind
			cur.net.kind = lower
			addPoll()
			cur.net.kind = prev
		case lower == "poll":
			addPoll()
		case name == "wait" && hasArg:
			d, err := time.ParseDuration(arg)
			if err != nil || d <= 0 {
				return nil, bad("want a positive duration like wait:30m")
			}
			n := int((d + pollEvery - 1) / pollEvery)
			for range n {
				p.polls = append(p.polls, cur)
			}
			pendingSettings = false

		case lower == "ac:on" || lower == "ac:off":
			cur.power.OnAC = arg == "on"
			pendingSettings = true
		case name == "batt" && hasArg:
			n, err := strconv.Atoi(arg)
			if err != nil || n < 0 || n > 100 {
				return nil, bad("want batt:0..100")
			}
			cur.power.Percent = n
			pendingSettings = true
		case lower == "lid:open" || lower == "lid:closed":
			cur.power.LidClosed = arg == "closed"
			pendingSettings = true
		case lower == "unplug" || lower == "plug":
			cur.net.unplugged = lower == "unplug"
			pendingSettings = true
		case lower == "cpu:none":
			cur.thermal.cpu = nil
			pendingSettings = true
		case name == "cpu" && hasArg:
			v, err := strconv.ParseFloat(arg, 64)
			if err != nil || v < 0 || v > 150 {
				return nil, bad("want cpu:<°C> or cpu:none")
			}
			cur.thermal.cpu = &v
			pendingSettings = true
		case lower == "btemp:none":
			cur.thermal.battC = nil
			p.btemp, pendingSettings = true, true
		case name == "btemp" && hasArg:
			v, err := strconv.ParseFloat(arg, 64)
			if err != nil || v < 0 || v > 100 {
				return nil, bad("want btemp:<°C> or btemp:none")
			}
			cur.thermal.battC = &v
			p.btemp, pendingSettings = true, true

		case lower == "stop" || lower == "stop:sleep" || lower == "stop:nosleep" || lower == "lease:lost" || name == "extend":
			if len(p.polls) == 0 {
				return nil, bad("commands run after a poll; put a poll token first")
			}
			c := command{token: lower}
			switch {
			case lower == "stop":
				c.stop = true
			case lower == "stop:sleep":
				c.stop, c.sleep = true, ptr(true)
			case lower == "stop:nosleep":
				c.stop, c.sleep = true, ptr(false)
			case lower == "lease:lost":
				c.lost = true
			case arg == "forever":
				c.extend, c.forever = true, true
			default:
				n, err := strconv.Atoi(arg)
				if err != nil || n < 1 {
					return nil, bad("want extend:<minutes> or extend:forever")
				}
				c.extend, c.minutes = true, n
			}
			idx := len(p.polls) - 1
			p.cmds[idx] = append(p.cmds[idx], c)

		case name == "for" && hasArg, lower == "forever", name == "hotspot", name == "watchdog" && hasArg:
			if len(p.polls) > 0 {
				return nil, bad("session tokens must come before the first poll")
			}
			switch {
			case lower == "forever":
				p.forever, p.duration = true, 0
			case lower == "hotspot":
				p.hotspot = config.HotspotOn
			case name == "hotspot":
				h, err := config.ParseHotspotPref(arg)
				if err != nil || !hasArg {
					return nil, bad("want hotspot:auto, hotspot:on or hotspot:off")
				}
				p.hotspot = h
			case name == "watchdog":
				if arg != "on" && arg != "off" {
					return nil, bad("want watchdog:on or watchdog:off")
				}
				p.watchdog = ptr(arg == "on")
			default:
				d, err := time.ParseDuration(arg)
				if err != nil || d <= 0 {
					return nil, bad("want for:<duration> like for:90m")
				}
				p.forever, p.duration = false, d
			}
		default:
			return nil, bad("unknown token")
		}
	}
	if len(p.polls) == 0 {
		return nil, fmt.Errorf("the script has no poll tokens")
	}
	if pendingSettings {
		return nil, fmt.Errorf("settings at the end of the script never take effect; add a poll token after them")
	}
	return p, nil
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func ptr[T any](v T) *T { return &v }
