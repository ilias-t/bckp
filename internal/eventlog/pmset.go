package eventlog

import (
	"bufio"
	"fmt"
	"io"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// macOS power-log events (src "macos"), parsed from `pmset -g log`.
const (
	EventPMSleep    = "pm.sleep"    // Sleep: entering Sleep or DarkWake, with the reason
	EventPMWake     = "pm.wake"     // Wake: full wake (from sleep or DarkWake)
	EventPMDarkWake = "pm.darkwake" // DarkWake: background wake, display off
	EventPMThermal  = "pm.thermal"  // ThermalEvent: e.g. an ignored thermal emergency
	EventPMBoot     = "pm.boot"     // Start: powerd started (boot)
)

// pmsetTypes maps the pmset log's type column to event names. Everything
// else (Assertions, Wake Requests, PM/Kernel Client Acks, WakeDetails,
// HibernateStats, ...) is ignored; note that several of those mention
// "Sleep" or "Wake" in their message, so matching must use the column.
var pmsetTypes = map[string]string{
	"Sleep":        EventPMSleep,
	"Wake":         EventPMWake,
	"DarkWake":     EventPMDarkWake,
	"ThermalEvent": EventPMThermal,
	"Start":        EventPMBoot,
}

// pmsetTimeLayout is the timestamp at the start of each pmset log line.
const pmsetTimeLayout = "2006-01-02 15:04:05 -0700"

var (
	pmsetPowerRE = regexp.MustCompile(`Using (AC|Batt|BATT)(?:\s*\(Charge:\s*(\d+)%?\))?`)
	pmsetSecsRE  = regexp.MustCompile(`\s(\d+) secs$`)
	pmsetEnterRE = regexp.MustCompile(`^Entering (\S+) state due to '([^']*)'`)
	pmsetDueRE   = regexp.MustCompile(`^(.*?)(?:\s*\[[A-Z]*\])?\s*:\s*due to\s*(.*?)(?:\s+Using\s|$)`)
	spacesRE     = regexp.MustCompile(`\s+`)
)

// ParsePmsetLog parses `pmset -g log` output and returns its Sleep, Wake,
// DarkWake, ThermalEvent and Start lines as events with src "macos",
// oldest first, keeping those in [since, until) (zero bounds are open).
//
// A Sleep caused by 'Clamshell Sleep' is logged at Warn: it is the lid
// sleep backpack exists to prevent, so it shows up in `logs --level warn`.
func ParsePmsetLog(r io.Reader, since, until time.Time) ([]Event, error) {
	var out []Event
	br := bufio.NewReaderSize(r, 256<<10)
	for {
		line, err := br.ReadString('\n')
		if ev, ok := parsePmsetLine(line); ok {
			if (since.IsZero() || !ev.Time.Before(since)) && (until.IsZero() || ev.Time.Before(until)) {
				out = append(out, ev)
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return out, err
		}
	}
	SortEvents(out)
	return out, nil
}

func parsePmsetLine(line string) (Event, bool) {
	// "2026-09-28 17:23:51 -0400 Sleep               \tEntering Sleep ..."
	const tsLen = len(pmsetTimeLayout)
	if len(line) < tsLen+2 || line[4] != '-' || line[10] != ' ' || line[tsLen] != ' ' {
		return Event{}, false
	}
	tab := strings.IndexByte(line, '\t')
	if tab < tsLen {
		return Event{}, false
	}
	name, ok := pmsetTypes[strings.TrimSpace(line[tsLen+1:tab])]
	if !ok {
		return Event{}, false
	}
	t, err := time.Parse(pmsetTimeLayout, line[:tsLen])
	if err != nil {
		return Event{}, false
	}
	// The message column may be followed by tab-separated Duration/Delay
	// columns (blank for these types).
	msg := line[tab+1:]
	if i := strings.IndexByte(msg, '\t'); i >= 0 {
		msg = msg[:i]
	}
	msg = strings.TrimSpace(spacesRE.ReplaceAllString(msg, " "))

	ev := Event{Time: t, Level: slog.LevelInfo, Src: SrcMacOS, Name: name}
	var summary string
	var attrs []Attr
	var power string
	if m := pmsetPowerRE.FindStringSubmatch(msg); m != nil {
		power = "AC"
		if m[1] != "AC" {
			power = "battery"
		}
		attrs = append(attrs, Attr{"power", power})
		if m[2] != "" {
			n, _ := strconv.Atoi(m[2])
			attrs = append(attrs, Attr{"charge", n})
			power += " " + m[2] + "%"
		}
	}
	var secs string
	if m := pmsetSecsRE.FindStringSubmatch(msg); m != nil {
		n, _ := strconv.Atoi(m[1])
		attrs = append(attrs, Attr{"secs", n})
		secs = m[1] + "s"
	}

	switch name {
	case EventPMSleep:
		if m := pmsetEnterRE.FindStringSubmatch(msg); m != nil {
			attrs = append([]Attr{{"state", m[1]}, {"reason", m[2]}}, attrs...)
			summary = fmt.Sprintf("%s due to '%s'", m[1], m[2])
			if m[2] == "Clamshell Sleep" {
				ev.Level = slog.LevelWarn
			}
		}
	case EventPMWake, EventPMDarkWake:
		if m := pmsetDueRE.FindStringSubmatch(msg); m != nil {
			reason := strings.Trim(strings.TrimSpace(m[2]), "/")
			attrs = append([]Attr{{"from", m[1]}, {"reason", reason}}, attrs...)
			summary = fmt.Sprintf("%s: %s", m[1], reason)
		}
	case EventPMThermal:
		ev.Level = slog.LevelWarn
	case EventPMBoot:
		summary = "powerd started (boot)"
	}
	if summary == "" {
		summary = msg
	}
	for _, s := range []string{power, secs} {
		if s != "" {
			summary += " · " + s
		}
	}
	ev.Msg = summary
	ev.Attrs = append(attrs, Attr{"raw", msg})
	return ev, true
}
