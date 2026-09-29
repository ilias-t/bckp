package eventlog

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"
)

// Format renders ev as one timeline line in loc:
//
//	21:24:03 engine  thermal.transition  NOMINAL→FAIR cpu=71°C
//	21:31:10 daemon  lease.renew_fail    WARN err="i/o timeout"
//	17:23:51 macos   pm.sleep            WARN Sleep due to 'Clamshell Sleep' · AC 100% · 223s
//
// Fields are shown in file order as key=value, with from/to as FROM→TO,
// *_c numbers as °C and battery/*_pct as percentages. macOS events show
// only their summary. Levels other than Info are tagged.
func Format(ev Event, loc *time.Location) string {
	var b strings.Builder
	b.WriteString(ev.Time.In(loc).Format("15:04:05"))
	b.WriteByte(' ')
	fmt.Fprintf(&b, "%-7s ", ev.Src)
	fmt.Fprintf(&b, "%-19s", ev.Name)
	b.WriteString(" ")
	b.WriteString(Summary(ev))
	return strings.TrimRight(b.String(), " ")
}

// Summary is Format without the time, src and event columns.
func Summary(ev Event) string {
	var parts []string
	if ev.Level != slog.LevelInfo {
		parts = append(parts, ev.Level.String())
	}
	if ev.Msg != "" && ev.Msg != ev.Name {
		parts = append(parts, ev.Msg)
	}
	if ev.Src == SrcMacOS {
		return strings.Join(parts, " ")
	}
	var from, to any
	var hasFrom, hasTo bool
	for _, a := range ev.Attrs {
		switch a.Key {
		case "from":
			from, hasFrom = a.Value, true
		case "to":
			to, hasTo = a.Value, true
		}
	}
	if hasFrom && hasTo {
		parts = append(parts, plain(from)+"→"+plain(to))
	}
	for _, a := range ev.Attrs {
		if hasFrom && hasTo && (a.Key == "from" || a.Key == "to") {
			continue
		}
		parts = append(parts, formatAttr(a))
	}
	return strings.Join(parts, " ")
}

func formatAttr(a Attr) string {
	if n, ok := number(a.Value); ok {
		switch {
		case strings.HasSuffix(a.Key, "_c"):
			return strings.TrimSuffix(a.Key, "_c") + "=" + strconv.FormatFloat(n, 'f', 0, 64) + "°C"
		case a.Key == "battery" || strings.HasSuffix(a.Key, "_pct"):
			return strings.TrimSuffix(a.Key, "_pct") + "=" + strconv.FormatFloat(n, 'f', -1, 64) + "%"
		}
	}
	return a.Key + "=" + quoted(a.Value)
}

func number(v any) (float64, bool) {
	switch n := v.(type) {
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	}
	return 0, false
}

// plain renders a value without quotes.
func plain(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case nil:
		return "null"
	case json.Number:
		return x.String()
	case bool, int, int64, float64:
		return fmt.Sprint(x)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}

// quoted renders a value, quoting strings that are empty or contain
// spaces, quotes or '='.
func quoted(v any) string {
	s, ok := v.(string)
	if !ok {
		return plain(v)
	}
	if s == "" || strings.ContainsAny(s, " \t\n\"=") {
		return strconv.Quote(s)
	}
	return s
}
