package tui

import (
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"

	"github.com/ilias-t/bckp/internal/contracts"
	"github.com/ilias-t/bckp/internal/humanize"
)

// Layout constants.
const (
	defaultWidth = 80
	// minWidth is the narrowest layout the dashboard is designed for;
	// narrower terminals still render, clipped per line.
	minWidth   = 60
	labelWidth = 9 // "NETWORK" + 2 spaces
	// maxNotifications is how many recent notifications are listed.
	maxNotifications = 5
)

// styles holds every lipgloss style the renderer uses. plain is true when
// the color profile is Ascii (NO_COLOR, a dumb terminal, or not a TTY);
// the renderer then uses textual markers instead of colored backgrounds.
type styles struct {
	plain bool

	bold, dim, ok, warn, bad, crit lipgloss.Style
	badges                         map[contracts.ThermalState]lipgloss.Style
}

// Palette (ANSI 256; lipgloss downsamples for 16-color terminals).
var (
	colGreen  = lipgloss.Color("34")
	colYellow = lipgloss.Color("220")
	colOrange = lipgloss.Color("208")
	colRed    = lipgloss.Color("196")
	colGray   = lipgloss.Color("245")
	colBlack  = lipgloss.Color("16")
)

// newStyles builds styles for a color profile.
func newStyles(p termenv.Profile) styles {
	r := lipgloss.NewRenderer(io.Discard)
	r.SetColorProfile(p)
	badge := func(bg lipgloss.Color) lipgloss.Style {
		return r.NewStyle().Bold(true).Foreground(colBlack).Background(bg)
	}
	return styles{
		plain: p == termenv.Ascii,
		bold:  r.NewStyle().Bold(true),
		dim:   r.NewStyle().Faint(true),
		ok:    r.NewStyle().Foreground(colGreen),
		warn:  r.NewStyle().Foreground(colOrange),
		bad:   r.NewStyle().Foreground(colRed).Bold(true),
		crit:  r.NewStyle().Foreground(colRed).Bold(true),
		badges: map[contracts.ThermalState]lipgloss.Style{
			contracts.ThermalUnknown:  badge(colGray),
			contracts.ThermalNominal:  badge(colGreen),
			contracts.ThermalFair:     badge(colYellow),
			contracts.ThermalSerious:  badge(colOrange),
			contracts.ThermalCritical: badge(colRed),
		},
	}
}

// render draws the whole screen for m. It is a pure function of the model
// (no clock reads, no I/O), so it is tested with golden files.
func render(m model) string {
	w := m.width
	if w <= 0 {
		w = defaultWidth
	}
	st := m.styles

	var lines []string
	add := func(ls ...string) { lines = append(lines, ls...) }

	add(renderHeader(m, w))
	add(st.dim.Render(strings.Repeat("─", w)))

	if m.showHelp {
		add(renderHelp(m)...)
		return clipLines(lines, w)
	}

	if b := renderConnBanner(m); b != "" {
		add(b, "")
	}

	s := m.snap
	if s == nil {
		if m.conn != connLost {
			add("connecting to engine…")
		}
		add("", renderFooter(m, w))
		return clipLines(lines, w)
	}

	if s.Session.Ended {
		add(st.bold.Render("Session ended: "+endReasonText(s.Session.EndReason)), "")
	}

	add(renderHeat(m, w)...)
	add(renderPower(m, w)...)
	add(renderNetwork(m, w)...)
	add(renderNotifications(m, w)...)
	add(renderErrors(m, w)...)
	add("", renderFooter(m, w))
	return clipLines(lines, w)
}

func renderHeader(m model, w int) string {
	st := m.styles
	left := st.bold.Render("backpack")
	s := m.snap
	if s == nil {
		return left
	}
	var state string
	switch {
	case s.Session.Ended:
		state = st.dim.Render("ended")
	case s.SleepPending != nil:
		sty := st.warn
		if !s.SleepPending.Abortable {
			sty = st.crit
		}
		state = sty.Render("sleep pending " + fmtClock(m.countdown(s.SleepPending.RemainingS)))
	case s.Thermal.FSM == contracts.FSMSleeping:
		state = st.crit.Render("sleeping")
	default:
		state = st.ok.Render("awake")
	}
	parts := []string{left, state}
	if !s.Session.Ended {
		parts = append(parts, sessionTime(m, s))
	}
	out := strings.Join(parts, "  ")
	if pid := fmt.Sprintf("pid %d", s.PID); s.PID > 0 {
		gap := w - ansi.StringWidth(out) - len(pid)
		if gap >= 2 {
			out += strings.Repeat(" ", gap) + st.dim.Render(pid)
		}
	}
	return out
}

// sessionTime is "1h23m left (ends 14:05)" or "forever (cap 08:15)".
func sessionTime(m model, s *contracts.Snapshot) string {
	ss := s.Session
	capAt := m.clockTime(ss.CapAt)
	if ss.Forever {
		return "forever (cap " + capAt + ")"
	}
	left := humanize.Duration(time.Duration(m.countdown(ss.RemainingS)) * time.Second)
	if !ss.CapAt.IsZero() && ss.CapAt.Before(ss.EndsAt) {
		return left + " left (cap " + capAt + ")"
	}
	return left + " left (ends " + m.clockTime(ss.EndsAt) + ")"
}

func renderConnBanner(m model) string {
	st := m.styles
	switch m.conn {
	case connLost:
		msg := "⚠ connection lost"
		if m.retrying {
			msg += " — reconnecting…"
		} else if !m.retryAt.IsZero() {
			wait := m.retryAt.Sub(m.now)
			if wait < 0 {
				wait = 0
			}
			msg += fmt.Sprintf(" — retry in %s (attempt %d)", humanize.Duration(wait.Round(time.Second)), m.attempt)
		}
		if m.lastErr != "" {
			msg += ": " + m.lastErr
		}
		return st.bad.Render(msg)
	}
	return ""
}

func renderHeat(m model, w int) []string {
	st := m.styles
	s := m.snap
	th := s.Thermal
	var fields []string
	state := contracts.ThermalUnknown
	if th.Reading != nil {
		state = th.Reading.State
	}
	fields = append(fields, badge(st, state))
	var cpu, gpu *float64
	if th.Reading != nil {
		cpu, gpu = th.Reading.CPUC, th.Reading.GPUC
	}
	// Without macmon there are no temperatures at all: show none rather
	// than "--" placeholders.
	if cpu != nil || gpu != nil {
		fields = append(fields, tempField(st, "CPU", cpu), tempField(st, "GPU", gpu))
	}
	if th.Reading != nil && th.Reading.BatteryC != nil {
		fields = append(fields, tempField(st, "BATT", th.Reading.BatteryC))
	}
	fsm := string(th.FSM)
	if fsm == "" {
		fsm = "?"
	}
	fields = append(fields, "fsm "+fsm)
	if !th.HotSince.IsZero() {
		fields = append(fields, st.warn.Render("hot "+humanize.Duration(m.since(th.HotSince))))
	}
	lines := wrapFields("HEAT", fields, w)

	if len(m.history) > 0 {
		lines = append(lines, strings.Repeat(" ", labelWidth)+cpuSpark(m.history, w-labelWidth))
	}

	if sp := s.SleepPending; sp != nil && !s.Session.Ended {
		msg := fmt.Sprintf("sleep in %s", fmtClock(m.countdown(sp.RemainingS)))
		if sp.Reason != "" {
			msg += " — " + sp.Reason
		}
		sty := st.warn
		if sp.Abortable {
			msg += " (x to abort)"
		} else {
			sty = st.crit
			msg += " (" + string(sp.Trigger) + ", cannot be aborted)"
		}
		lines = append(lines, strings.Repeat(" ", labelWidth)+sty.Render("⏾ "+msg))
	}
	return lines
}

// cpuSpark is "cpu ▁▂▃▅ 48–56°C", keeping only the newest samples that
// fit in avail columns.
func cpuSpark(hist []float64, avail int) string {
	lo, hi := minMax(hist)
	rng := fmt.Sprintf(" %.0f–%.0f°C", lo, hi)
	n := avail - len("cpu ") - ansi.StringWidth(rng)
	if n < 4 {
		n = 4
	}
	if len(hist) > n {
		hist = hist[len(hist)-n:]
		lo, hi = minMax(hist)
		rng = fmt.Sprintf(" %.0f–%.0f°C", lo, hi)
	}
	return "cpu " + sparkline(hist) + rng
}

func badge(st styles, s contracts.ThermalState) string {
	name := strings.ToUpper(s.String())
	if st.plain {
		return "[" + name + "]"
	}
	return st.badges[s].Render(" " + name + " ")
}

func tempField(st styles, label string, c *float64) string {
	if c == nil {
		return st.dim.Render(label + " --")
	}
	return fmt.Sprintf("%s %.1f°C", label, *c)
}

func renderPower(m model, w int) []string {
	st := m.styles
	s := m.snap
	var fields []string
	p := s.Power
	if p == nil {
		fields = append(fields, st.dim.Render("no reading yet"))
	} else {
		src := fmt.Sprintf("AC %d%%", p.Percent)
		if !p.OnAC {
			src = st.warn.Render(fmt.Sprintf("battery %d%%", p.Percent))
		}
		if p.Charging {
			src += " charging"
		}
		lid := "lid open"
		if p.LidClosed {
			lid = "lid closed"
		}
		sd := st.ok.Render("SleepDisabled yes")
		if !p.SleepDisabled {
			sd = st.warn.Render("SleepDisabled no")
		}
		fields = append(fields, src, lid, sd)
	}
	if s.Lease.Held {
		fields = append(fields, st.ok.Render("lease held"))
	} else {
		fields = append(fields, st.bad.Render("lease NOT held"))
	}
	lines := wrapFields("POWER", fields, w)
	if s.Session.Ended {
		return lines
	}
	indent := strings.Repeat(" ", labelWidth)
	switch {
	case !s.Lease.Held:
		lines = append(lines, indent+st.bad.Render("⚠ sleep lease not held — closing the lid may sleep the Mac"))
	case p != nil && !p.SleepDisabled:
		lines = append(lines, indent+st.warn.Render("⚠ lease held but the kernel reports SleepDisabled=No"))
	}
	return lines
}

func renderNetwork(m model, w int) []string {
	st := m.styles
	n := m.snap.Network
	if n.Mode == contracts.NetworkOff || n.Mode == "" {
		return wrapFields("NETWORK", []string{st.dim.Render("off")}, w)
	}
	var fields []string
	l := n.Latest
	if l == nil {
		fields = append(fields, st.dim.Render("no probe yet"))
	} else {
		iface := l.Iface
		if iface == "" {
			iface = "iface ?"
		}
		fields = append(fields, iface)
	}
	switch {
	case l == nil:
		// No probe yet: Online is meaningless.
	case n.Online:
		on := "online"
		if link := l.Link(); link != contracts.LinkNone {
			on += " via " + link.Label()
		}
		fields = append(fields, st.ok.Render(on))
	default:
		off := "OFFLINE"
		if !n.OfflineSince.IsZero() {
			off += " " + humanize.Duration(m.since(n.OfflineSince))
		}
		fields = append(fields, st.bad.Render(off))
	}
	if l != nil {
		fields = append(fields,
			yesNo(st, "gw reachable", "gw unreachable", l.GatewayReachable),
			yesNo(st, "iPhone USB", "no iPhone USB", l.IPhoneUSBPresent),
			"Wi-Fi "+humanize.OnOff(l.WiFiOn))
	}
	switch n.Mode {
	case contracts.NetworkPreferHotspot:
		fields = append(fields, "hotspot preferred")
	case contracts.NetworkHotspotAuto:
		fields = append(fields, "hotspot auto: waiting for the phone")
	case contracts.NetworkWatchdog:
		fields = append(fields, "watchdog only")
	default:
		fields = append(fields, string(n.Mode))
	}
	rec := fmt.Sprintf("recoveries %d", n.Recoveries)
	if !n.LastRecovery.IsZero() {
		rec += " (last " + m.clockTime(n.LastRecovery) + ")"
	}
	fields = append(fields, rec)
	if n.WiFiNudges > 0 {
		last := n.LastWiFiNudgeResult
		if last == "" {
			last = "pending"
		}
		fields = append(fields, fmt.Sprintf("Wi-Fi nudges %d (last %s %s)", n.WiFiNudges, m.clockTime(n.LastWiFiNudge), last))
	}
	return wrapFields("NETWORK", fields, w)
}

func yesNo(st styles, yes, no string, v bool) string {
	if v {
		return yes
	}
	return st.warn.Render(no)
}

func renderNotifications(m model, w int) []string {
	st := m.styles
	s := m.snap
	head := "NOTIFY"
	var extra []string
	if s.OutboxPending > 0 {
		extra = append(extra, st.warn.Render(fmt.Sprintf("%d queued in outbox", s.OutboxPending)))
	}
	ns := s.Notifications
	if len(ns) == 0 {
		return wrapFields(head, append([]string{st.dim.Render("none yet")}, extra...), w)
	}
	var lines []string
	if len(extra) > 0 {
		lines = wrapFields(head, extra, w)
		head = ""
	}
	// Newest first.
	for i := len(ns) - 1; i >= 0 && len(ns)-i <= maxNotifications; i-- {
		r := ns[i]
		prio := r.Notification.Priority
		if prio == 0 {
			prio = contracts.PriorityDefault
		}
		p := fmt.Sprintf("p%d", prio)
		switch {
		case prio >= contracts.PriorityUrgent:
			p = st.crit.Render(p)
		case prio == contracts.PriorityHigh:
			p = st.warn.Render(p)
		case prio <= contracts.PriorityLow:
			p = st.dim.Render(p)
		}
		switch r.Status {
		case contracts.NotificationQueued:
			p += " " + st.warn.Render("(queued)")
		case contracts.NotificationFailed:
			p += " " + st.bad.Render("(failed)")
		case contracts.NotificationLogged:
			// Info with notify.push = "attention": not sent to the phone.
			p += " " + st.dim.Render("(log only)")
		}
		text := r.Notification.Title
		if body := firstLine(r.Notification.Body); body != "" {
			if text != "" {
				text += " — "
			}
			text += body
		}
		line := padLabel(head) + m.clockTime(r.At) + " " + p + " " + text
		lines = append(lines, line)
		head = ""
	}
	return lines
}

func renderErrors(m model, w int) []string {
	st := m.styles
	errs := m.snap.Errors
	if len(errs) == 0 {
		return nil
	}
	keys := make([]string, 0, len(errs))
	for k := range errs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var lines []string
	head := "ERRORS"
	for _, k := range keys {
		lines = append(lines, padLabel(head)+st.bad.Render(k+": "+firstLine(errs[k])))
		head = ""
	}
	return lines
}

func renderFooter(m model, w int) string {
	st := m.styles
	s := m.snap
	var status string
	switch {
	case s != nil && s.Session.Ended:
		return st.bold.Render("press any key to exit")
	case m.confirmStop:
		q := st.warn.Render("Stop the session? [y/N]")
		if m.confirmQuit {
			q = st.warn.Render(ForegroundQuitText)
		}
		if s != nil && s.SleepPending != nil {
			if s.SleepPending.Abortable {
				q += st.dim.Render("  this cancels the pending sleep")
			} else {
				q += st.dim.Render("  the critical sleep still runs")
			}
		}
		return q
	case m.flash != "":
		if m.flashErr {
			status = st.bad.Render(m.flash)
		} else {
			status = st.ok.Render(m.flash)
		}
	}
	keys := st.dim.Render(keyHints(w, m.foreground))
	if status == "" {
		return keys
	}
	return status + "\n" + keys
}

func keyHints(w int, foreground bool) string {
	q := "q detach"
	if foreground {
		q = "q stop (-f)"
	}
	if w < 72 {
		return q + " · s stop · e +30m · f forever · x abort · ? help"
	}
	return q + " · s stop · e +30m · f forever · x abort sleep · ? help"
}

func renderHelp(m model) []string {
	st := m.styles
	rows := [][2]string{
		{"q", qHelp(m.foreground)},
		{"s, ctrl+c", "stop the session (asks to confirm)"},
		{"e", fmt.Sprintf("extend by %d minutes (also cancels a serious-heat sleep)", extendMinutes)},
		{"f", "run forever (until the max_duration cap)"},
		{"x", "abort a pending serious-heat sleep (no time added)"},
		{"?", "toggle this help"},
	}
	lines := []string{st.bold.Render("Keys"), ""}
	for _, r := range rows {
		lines = append(lines, fmt.Sprintf("  %-10s %s", r[0], r[1]))
	}
	lines = append(lines, "",
		"A sleep triggered by critical heat cannot be aborted or postponed.",
		"Reattach later with `bckp attach`.",
		"", st.dim.Render("press any key to close"))
	return lines
}

func qHelp(foreground bool) string {
	if foreground {
		return "stop (asks to confirm): this is a foreground (-f) session; run without -f to detach"
	}
	return "detach: close this view; the session keeps running"
}

// wrapFields lays out fields after a fixed-width label, separated by two
// spaces, wrapping onto indented continuation lines to fit width w.
func wrapFields(label string, fields []string, w int) []string {
	var lines []string
	cur := padLabel(label)
	curW := labelWidth
	empty := true
	for _, f := range fields {
		fw := ansi.StringWidth(f)
		if !empty && curW+2+fw > w {
			lines = append(lines, cur)
			cur, curW, empty = strings.Repeat(" ", labelWidth), labelWidth, true
		}
		if !empty {
			cur += "  "
			curW += 2
		}
		cur += f
		curW += fw
		empty = false
	}
	return append(lines, cur)
}

func padLabel(label string) string {
	return label + strings.Repeat(" ", max(labelWidth-len(label), 1))
}

// clipLines joins lines, truncating each to width w and trimming trailing
// spaces.
func clipLines(lines []string, w int) string {
	var out []string
	for _, l := range lines {
		for _, sub := range strings.Split(l, "\n") {
			if ansi.StringWidth(sub) > w {
				sub = ansi.Truncate(sub, w, "…")
			}
			out = append(out, strings.TrimRight(sub, " "))
		}
	}
	return strings.Join(out, "\n")
}

var sparkBlocks = []rune("▁▂▃▄▅▆▇█")

// sparkline renders values scaled between their min and max. A flat series
// renders mid-height.
func sparkline(vs []float64) string {
	lo, hi := minMax(vs)
	var b strings.Builder
	for _, v := range vs {
		i := len(sparkBlocks) / 2
		if hi > lo {
			i = int(math.Round((v - lo) / (hi - lo) * float64(len(sparkBlocks)-1)))
		}
		b.WriteRune(sparkBlocks[i])
	}
	return b.String()
}

func minMax(vs []float64) (lo, hi float64) {
	lo, hi = math.Inf(1), math.Inf(-1)
	for _, v := range vs {
		lo, hi = math.Min(lo, v), math.Max(hi, v)
	}
	return lo, hi
}

// fmtClock formats whole seconds as m:ss.
func fmtClock(secs int64) string {
	if secs < 0 {
		secs = 0
	}
	return fmt.Sprintf("%d:%02d", secs/60, secs%60)
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i]) + " …"
	}
	return s
}

func endReasonText(r contracts.EndReason) string {
	switch r {
	case contracts.EndStopped:
		return "stopped"
	case contracts.EndSignal:
		return "stopped by a signal"
	case contracts.EndExpired:
		return "requested time elapsed"
	case contracts.EndMaxDuration:
		return "max_duration cap reached"
	case contracts.EndBatteryFloor:
		return "battery reached the floor"
	case contracts.EndThermalSleep:
		return "put the Mac to sleep to protect it from heat"
	case contracts.EndUserSleep:
		return "put the Mac to sleep (stop --sleep)"
	case contracts.EndLeaseLost:
		return "sleep lease lost"
	case contracts.EndError:
		return "engine error"
	case "":
		return "unknown reason"
	}
	return string(r)
}
