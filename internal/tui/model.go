package tui

import (
	"context"
	"errors"
	"fmt"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/ilias-t/bckp/internal/contracts"
	"github.com/ilias-t/bckp/internal/humanize"
)

const (
	// extendMinutes is what `e` adds.
	extendMinutes = 30
	// historyLen is how many CPU °C samples the sparkline keeps.
	historyLen = 40
	// actionTimeout bounds Stop/Extend calls.
	actionTimeout = 15 * time.Second
	// flashTTL is how long an action result stays in the footer.
	flashTTL = 8 * time.Second
	// maxBackoff caps the reconnect delay.
	maxBackoff = 10 * time.Second
)

type connState int

const (
	connConnecting connState = iota // before the first snapshot
	connLive                        // receiving snapshots
	connLost                        // stream closed without Ended; retrying
	connEnded                       // received a snapshot with Session.Ended
)

// exitKind says why the TUI quit, so Run can print the right line.
type exitKind int

const (
	exitNone     exitKind = iota
	exitDetached          // q: the engine keeps running
	exitEnded             // the session ended and the user pressed a key
	exitLost              // ctrl+c while disconnected
)

// Messages.
type (
	// snapMsg is one snapshot from subscription generation gen.
	snapMsg struct {
		gen  int
		snap contracts.Snapshot
	}
	// subClosedMsg reports that subscription gen's channel closed.
	subClosedMsg struct{ gen int }
	// subscribedMsg carries a new subscription.
	subscribedMsg struct {
		gen    int
		ch     <-chan contracts.Snapshot
		cancel context.CancelFunc
	}
	// subscribeFailedMsg reports a failed Subscribe.
	subscribeFailedMsg struct {
		gen int
		err error
	}
	// reconnectMsg fires when the backoff for generation gen elapsed.
	reconnectMsg struct{ gen int }
	// tickMsg drives local countdowns between snapshots.
	tickMsg time.Time
	// actionMsg is the result of Stop or Extend.
	actionMsg struct {
		kind actionKind
		snap *contracts.Snapshot
		err  error
	}
)

type actionKind int

const (
	actStop actionKind = iota
	actExtend
	actForever
	actAbort
)

// model is the bubbletea model. Rendering lives in render.go.
type model struct {
	ctx    context.Context
	client contracts.EngineClient

	// Subscription state. gen increases with every Subscribe attempt;
	// messages from older generations are ignored.
	gen       int
	sub       <-chan contracts.Snapshot
	subCancel context.CancelFunc
	conn      connState
	attempt   int       // consecutive failed (re)connects
	retryAt   time.Time // when the next reconnect fires (connLost)
	retrying  bool      // a Subscribe call is in flight
	lastErr   string    // last Subscribe error, shown in the banner
	backoff   func(attempt int) time.Duration

	snap   *contracts.Snapshot
	recvAt time.Time // local time snap arrived
	now    time.Time // local time of the last update; render reads this

	history       []float64 // recent CPU °C, oldest first
	lastReadingAt time.Time

	width  int
	styles styles
	loc    *time.Location
	clock  func() time.Time
	tick   time.Duration

	confirmStop bool
	// confirmQuit marks a confirmStop opened by q in a foreground session.
	confirmQuit bool
	// foreground: the engine runs in this process; q cannot detach.
	foreground bool
	showHelp   bool
	flash      string
	flashErr   bool
	flashAt    time.Time

	exit exitKind
}

// newModel builds a model. sub may be nil, in which case Init subscribes.
func newModel(ctx context.Context, c contracts.EngineClient, sub <-chan contracts.Snapshot, cancel context.CancelFunc, st styles) model {
	now := time.Now()
	return model{
		ctx:       ctx,
		client:    c,
		sub:       sub,
		subCancel: cancel,
		backoff:   defaultBackoff,
		styles:    st,
		loc:       time.Local,
		clock:     time.Now,
		tick:      time.Second,
		now:       now,
	}
}

// defaultBackoff is 500ms, 1s, 2s, 4s, 8s, then 10s.
func defaultBackoff(attempt int) time.Duration {
	d := 500 * time.Millisecond
	for i := 0; i < attempt && d < maxBackoff; i++ {
		d *= 2
	}
	return min(d, maxBackoff)
}

func (m model) Init() tea.Cmd {
	if m.sub == nil {
		return tea.Batch(subscribeCmd(m.ctx, m.client, m.gen), m.tickCmd())
	}
	return tea.Batch(waitCmd(m.sub, m.gen), m.tickCmd())
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		return m, nil

	case tickMsg:
		m.now = time.Time(msg)
		if m.flash != "" && m.now.Sub(m.flashAt) > flashTTL {
			m.flash = ""
		}
		return m, m.tickCmd()

	case snapMsg:
		if msg.gen != m.gen || m.conn == connEnded {
			return m, nil
		}
		m.now = m.clock()
		m.applySnapshot(msg.snap)
		m.attempt, m.lastErr, m.retrying = 0, "", false
		if msg.snap.Session.Ended {
			m.conn = connEnded
			m.confirmStop, m.showHelp = false, false
			m.cancelSub()
			return m, nil
		}
		m.conn = connLive
		return m, waitCmd(m.sub, m.gen)

	case subClosedMsg:
		if msg.gen != m.gen || m.conn == connEnded {
			return m, nil
		}
		m.cancelSub()
		return m.scheduleReconnect()

	case subscribeFailedMsg:
		if msg.gen != m.gen {
			return m, nil
		}
		m.lastErr = shortErr(msg.err)
		return m.scheduleReconnect()

	case subscribedMsg:
		if msg.gen != m.gen || m.conn == connEnded {
			msg.cancel()
			return m, nil
		}
		m.sub, m.subCancel = msg.ch, msg.cancel
		return m, waitCmd(m.sub, m.gen)

	case reconnectMsg:
		if msg.gen != m.gen || m.conn != connLost {
			return m, nil
		}
		m.gen++
		m.retrying = true
		return m, subscribeCmd(m.ctx, m.client, m.gen)

	case actionMsg:
		return m.applyAction(msg), nil

	case tea.KeyMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

func (m model) View() string { return render(m) }

func (m *model) applySnapshot(s contracts.Snapshot) {
	m.snap = &s
	m.recvAt = m.now
	r := s.Thermal.Reading
	if r == nil || r.CPUC == nil {
		return
	}
	if !r.At.IsZero() && r.At.Equal(m.lastReadingAt) {
		return // same poll re-sent on a state change
	}
	m.lastReadingAt = r.At
	m.history = append(m.history, *r.CPUC)
	if len(m.history) > historyLen {
		m.history = append(m.history[:0:0], m.history[len(m.history)-historyLen:]...)
	}
}

func (m *model) cancelSub() {
	if m.subCancel != nil {
		m.subCancel()
		m.subCancel = nil
	}
	m.sub = nil
}

func (m model) scheduleReconnect() (tea.Model, tea.Cmd) {
	m.conn = connLost
	m.retrying = false
	m.confirmStop = false
	d := m.backoff(m.attempt)
	m.attempt++
	m.now = m.clock()
	m.retryAt = m.now.Add(d)
	gen := m.gen
	return m, tea.Tick(d, func(time.Time) tea.Msg { return reconnectMsg{gen: gen} })
}

func (m model) handleKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	key := k.String()

	if m.conn == connEnded {
		m.exit = exitEnded
		return m, tea.Quit
	}

	if m.confirmStop {
		m.confirmStop, m.confirmQuit = false, false
		switch key {
		case "y", "Y", "enter", "ctrl+c":
			m.setFlash("stopping…", false)
			return m, m.stopCmd()
		}
		m.setFlash("stop cancelled", false)
		return m, nil
	}

	if m.showHelp && key != "q" && key != "ctrl+c" {
		m.showHelp = false
		return m, nil
	}

	switch key {
	case "q":
		if m.foreground {
			if m.conn != connLive {
				m.exit = exitLost
				return m, tea.Quit
			}
			m.confirmStop, m.confirmQuit, m.showHelp = true, true, false
			return m, nil
		}
		m.exit = exitDetached
		return m, tea.Quit
	case "ctrl+c":
		if m.conn != connLive {
			m.exit = exitLost
			return m, tea.Quit
		}
		m.confirmStop = true
		return m, nil
	case "?":
		m.showHelp = true
		return m, nil
	case "s", "e", "f", "x":
		if m.conn != connLive {
			m.setFlash("not connected to the engine", true)
			return m, nil
		}
	}

	switch key {
	case "s":
		m.confirmStop = true
		return m, nil
	case "e":
		return m, m.extendCmd(actExtend, extendMinutes, false)
	case "f":
		return m, m.extendCmd(actForever, 0, true)
	case "x":
		sp := (*contracts.SleepPending)(nil)
		if m.snap != nil {
			sp = m.snap.SleepPending
		}
		switch {
		case sp == nil:
			m.setFlash("no sleep pending", true)
			return m, nil
		case !sp.Abortable:
			m.setFlash(notAbortableText, true)
			return m, nil
		}
		return m, m.abortCmd()
	}
	return m, nil
}

const notAbortableText = "a critical-heat sleep is pending and cannot be aborted or postponed"

func (m *model) setFlash(s string, isErr bool) {
	m.flash, m.flashErr, m.flashAt = s, isErr, m.clock()
}

func (m model) applyAction(a actionMsg) model {
	if a.err != nil {
		msg := actionName(a.kind) + " failed: " + shortErr(a.err)
		switch {
		case errors.Is(a.err, contracts.ErrNotAbortable):
			msg = notAbortableText
		case errors.Is(a.err, contracts.ErrNoSleepPending):
			msg = "no sleep pending"
		}
		m.setFlash(msg, true)
		return m
	}
	switch a.kind {
	case actStop:
		m.setFlash("stop requested — waiting for the engine…", false)
	case actExtend:
		m.setFlash(fmt.Sprintf("extended by %dm", extendMinutes), false)
	case actForever:
		m.setFlash("now running forever (until the cap)", false)
	case actAbort:
		m.setFlash("pending sleep aborted", false)
	}
	if a.snap != nil && !a.snap.Session.Ended && m.conn == connLive &&
		(m.snap == nil || !a.snap.At.Before(m.snap.At)) {
		m.now = m.clock()
		m.applySnapshot(*a.snap)
	}
	return m
}

func actionName(k actionKind) string {
	switch k {
	case actStop:
		return "stop"
	case actExtend:
		return "extend"
	case actForever:
		return "forever"
	case actAbort:
		return "abort sleep"
	}
	return "action"
}

// shortErr strips the error to one line.
func shortErr(err error) string {
	if errors.Is(err, contracts.ErrEngineNotRunning) {
		return "engine not running"
	}
	return firstLine(err.Error())
}

// Commands.

func waitCmd(ch <-chan contracts.Snapshot, gen int) tea.Cmd {
	return func() tea.Msg {
		s, ok := <-ch
		if !ok {
			return subClosedMsg{gen: gen}
		}
		return snapMsg{gen: gen, snap: s}
	}
}

func subscribeCmd(ctx context.Context, c contracts.EngineClient, gen int) tea.Cmd {
	return func() tea.Msg {
		subCtx, cancel := context.WithCancel(ctx)
		ch, err := c.Subscribe(subCtx)
		if err != nil {
			cancel()
			return subscribeFailedMsg{gen: gen, err: err}
		}
		return subscribedMsg{gen: gen, ch: ch, cancel: cancel}
	}
}

func (m model) tickCmd() tea.Cmd {
	return tea.Tick(m.tick, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func (m model) stopCmd() tea.Cmd {
	ctx, c := m.ctx, m.client
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(ctx, actionTimeout)
		defer cancel()
		return actionMsg{kind: actStop, err: c.Stop(ctx, nil)}
	}
}

func (m model) extendCmd(kind actionKind, minutes int, forever bool) tea.Cmd {
	ctx, c := m.ctx, m.client
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(ctx, actionTimeout)
		defer cancel()
		s, err := c.Extend(ctx, minutes, forever)
		if err != nil {
			return actionMsg{kind: kind, err: err}
		}
		return actionMsg{kind: kind, snap: &s}
	}
}

// abortCmd cancels a pending abortable sleep (abort_sleep): no time is
// added and the cap is unchanged.
func (m model) abortCmd() tea.Cmd {
	ctx, c := m.ctx, m.client
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(ctx, actionTimeout)
		defer cancel()
		s, err := c.AbortSleep(ctx)
		if err != nil {
			return actionMsg{kind: actAbort, err: err}
		}
		return actionMsg{kind: actAbort, snap: &s}
	}
}

// Time helpers used by render. They read m.now, never the wall clock.

// elapsed is the local time since the current snapshot arrived.
func (m model) elapsed() time.Duration {
	if m.recvAt.IsZero() || m.now.Before(m.recvAt) {
		return 0
	}
	return m.now.Sub(m.recvAt)
}

// countdown adjusts a snapshot's remaining seconds by the local time since
// it arrived, so countdowns tick between snapshots.
func (m model) countdown(remainingS int64) int64 {
	return max(remainingS-int64(m.elapsed()/time.Second), 0)
}

// since is the duration from t to the engine's "now" (snapshot At plus
// local elapsed time).
func (m model) since(t time.Time) time.Duration {
	if m.snap == nil || t.IsZero() {
		return 0
	}
	return max(m.snap.At.Add(m.elapsed()).Sub(t), 0)
}

func (m model) clockTime(t time.Time) string {
	if t.IsZero() {
		return "--:--"
	}
	now := t
	if m.snap != nil {
		now = m.snap.At.Add(m.elapsed())
	}
	return humanize.Clock(t, now.In(m.loc))
}
