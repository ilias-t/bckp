package simulate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ilias-t/bckp/internal/config"
	"github.com/ilias-t/bckp/internal/contracts"
	"github.com/ilias-t/bckp/internal/engine"
	"github.com/ilias-t/bckp/internal/fakes"
	"github.com/ilias-t/bckp/internal/humanize"
	"github.com/ilias-t/bckp/internal/paths"
)

// Start is the fake clock's start time (a fixed time so output is stable).
var Start = time.Date(2026, 1, 1, 9, 0, 0, 0, time.UTC)

// parkTimeout bounds how long the driver waits for the engine to settle;
// the engine never waits on real time, so this only trips on a bug.
const parkTimeout = 10 * time.Second

// Run parses script (see Grammar), runs the real engine with cfg against
// fakes and a fake clock, and writes a timeline to out. It returns a parse
// error (wrapping contracts.ErrBadScript) or config error, or an engine error other than a scripted lease loss.
// The timeline is read back from the engine's structured event log.
func Run(ctx context.Context, script string, cfg config.Config, out io.Writer) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	p, err := parse(script, cfg.Session.Poll)
	if err != nil {
		return fmt.Errorf("%w: %w", contracts.ErrBadScript, err)
	}
	home, err := os.MkdirTemp("", "bckp-simulate")
	if err != nil {
		return err
	}
	defer os.RemoveAll(home)

	s := newSim(p, cfg, out)
	s.deps.Paths = paths.ForHome(home)
	s.e = engine.New(s.deps)
	return s.run(ctx, script)
}

// item is one buffered timeline entry.
type item struct {
	at   time.Time
	poll int    // 1-based poll number for a poll marker, else 0
	key  string // poll marker: the full input, for folding quiet polls
	tag  string
	text string
}

type sim struct {
	p     *plan
	cfg   config.Config
	out   io.Writer
	clock *fakes.Clock
	guard *fakes.SleepGuard
	net   *fakes.Network
	deps  engine.Deps
	e     *engine.Engine

	mu                             sync.Mutex
	items                          []item
	gen                            int
	next                           time.Time
	thermalReads, powerReads, nets int
	renews                         int
	notifyCount                    map[int]int
	pushed                         int // notifications the push policy would push
	guardCount                     map[string]int
	sig                            chan struct{}

	// Driver-owned.
	seen      int
	parked    bool
	parkedAt  time.Time
	lastTime  time.Time
	lastInput string // key of the last printed or folded poll
	lastState string
	quiet     *quietRun
	runErr    chan error
}

type quietRun struct {
	from, to int
	at       time.Time
	input    string
	state    string
}

func newSim(p *plan, cfg config.Config, out io.Writer) *sim {
	clock := fakes.NewClock(Start)
	s := &sim{
		p: p, cfg: cfg, out: out, clock: clock,
		guard:       fakes.NewSleepGuard(clock),
		net:         fakes.NewNetwork(clock, "Wi-Fi", cfg.Network.Service, "Thunderbolt Bridge"),
		notifyCount: map[int]int{},
		guardCount:  map[string]int{},
		sig:         make(chan struct{}, 1),
		runErr:      make(chan error, 1),
	}
	if p.hotspot != "" {
		cfg.Network.PreferHotspot = p.hotspot
	}
	if p.watchdog != nil {
		cfg.Network.Watchdog = *p.watchdog
	}
	s.cfg = cfg
	s.deps = engine.Deps{
		Clock:    clock,
		Thermal:  thermalSrc{s},
		Power:    powerSrc{s},
		Network:  &netSrc{Network: s.net, s: s},
		Notifier: fakes.NewNotifier(),
		Guard:    guardRec{g: s.guard, s: s},
		Config:   s.cfg,
		Logger:   fakes.NewLog(s.onRecord).Logger(),
		LoadAvg:  func() (float64, error) { return 2.0, nil },
		OnIdle:   s.onIdle,
	}
	return s
}

func (s *sim) input(i int) pollInput {
	if i >= len(s.p.polls) {
		i = len(s.p.polls) - 1
	}
	return s.p.polls[i]
}

func (s *sim) record(tag, text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.items = append(s.items, item{at: s.clock.Now(), tag: tag, text: text})
}

func (s *sim) onIdle(next time.Time) {
	s.mu.Lock()
	s.gen++
	s.next = next
	s.mu.Unlock()
	select {
	case s.sig <- struct{}{}:
	default:
	}
}

// onRecord renders the engine's event log records that belong on the
// timeline (the rest, like samples and FSM notes, stay out).
func (s *sim) onRecord(r fakes.Record) {
	svc := fmt.Sprintf("%q", s.cfg.Network.Service)
	switch r.Name {
	case "notify.logged", "notify.enqueue":
		pushed := r.Name == "notify.enqueue"
		mark := "[log only]"
		if pushed {
			mark = "[pushed]"
		}
		prio := int(r.Attrs["priority"].Int64())
		s.mu.Lock()
		s.notifyCount[prio]++
		if pushed {
			s.pushed++
		}
		s.mu.Unlock()
		s.record("notify", fmt.Sprintf("p%d %s %s", prio, r.Str("title"), mark))
	case "lease.lost":
		s.record("lease", "lease lost: "+r.Str("err"))
	case "session.ending":
		s.record("session", fmt.Sprintf("session ending: %s (%s)", r.Str("reason"), r.Str("detail")))
	case "session.extend":
		capAt := r.Str("cap_at")
		if t, err := time.Parse(time.RFC3339, capAt); err == nil {
			capAt = t.Format("15:04")
		}
		s.record("session", fmt.Sprintf("extended: %s, cap %s", r.Str("desc"), capAt))
	case "thermal.transition":
		s.record("thermal", r.Str("from")+" -> "+r.Str("to"))
	case "sleep.pending":
		s.record("thermal", fmt.Sprintf("sleep pending (%s): %s", r.Str("trigger"), r.Str("reason")))
	case "sleep.cancel":
		s.record("thermal", fmt.Sprintf("pending %s sleep cancelled by %s", r.Str("trigger"), r.Str("by")))
	case "sleep.failed":
		s.record("error", "sleep failed: "+r.Str("err"))
	case "net.transition":
		switch {
		case r.Str("to") == "offline":
			s.record("network", "offline")
		case r.Str("from") == "online":
			s.record("network", fmt.Sprintf("online: %s -> %s", r.Str("from_link"), r.Str("link")))
		default:
			s.record("network", "online via "+r.Str("link"))
		}
	case "net.order_change":
		text := svc + " already ahead of Wi-Fi"
		if r.Str("changed") == "true" {
			text = fmt.Sprintf("moved %s ahead of Wi-Fi: %s", svc, r.Str("to"))
		}
		if r.Str("trigger") == "auto" {
			text = "hotspot connected: " + text
		}
		s.record("network", text)
	case "net.setup_error":
		s.record("error", fmt.Sprintf("network: %s: %s", r.Str("step"), r.Str("err")))
	case "net.restore":
		switch {
		case r.Str("result") != "ok":
			s.record("error", "network restore failed: "+r.Str("err"))
		case r.Str("stale") == "true":
			s.record("network", "restored stale journal")
		default:
			s.record("network", "network restored from journal")
		}
	case "net.recovery_start":
		s.record("network", fmt.Sprintf("recovery: %s off (%s)", svc, r.Str("why")))
	case "net.recovery_attempt":
		switch r.Str("result") {
		case "ok":
			s.record("network", fmt.Sprintf("recovery: %s on", svc))
		case "refused":
			s.record("error", "recovery refused: "+r.Str("err"))
		default:
			s.record("error", fmt.Sprintf("recovery failed at %s: %s", r.Str("step"), r.Str("err")))
		}
	case "net.wifi_nudge_start":
		what := "Wi-Fi off"
		if r.Str("power_on_only") == "true" {
			what = "Wi-Fi is off, turning it on"
		}
		s.record("network", fmt.Sprintf("wifi nudge %s: %s (%s)", r.Str("attempt"), what, r.Str("why")))
	case "net.wifi_nudge":
		s.record("network", fmt.Sprintf("wifi nudge %s: %s (%s)", r.Str("attempt"), r.Str("result"), r.Str("outcome")))
	case "net.wifi_change":
		if r.Str("result") != "ok" {
			s.record("error", fmt.Sprintf("wifi on after nudge failed: %s (retrying)", r.Str("err")))
		}
	}
}

func (s *sim) run(ctx context.Context, script string) error {
	s.header(script)
	go func() {
		s.runErr <- s.e.Run(ctx, engine.Options{Duration: s.p.duration})
	}()

	poll := s.cfg.Session.Poll
	for i := range s.p.polls {
		if err := s.advanceTo(Start.Add(time.Duration(i) * poll)); err != nil {
			return err
		}
		s.flush()
		for _, c := range s.p.cmds[i] {
			if err := s.command(ctx, c); err != nil {
				return err
			}
			s.flush()
		}
		if s.done() {
			break
		}
	}
	if !s.done() {
		s.flushQuiet()
		s.line(s.clock.Now(), "script", "end of script")
		// Let a pending sleep play out; otherwise stop like `bckp stop`.
		for !s.done() {
			p := s.e.Snapshot().SleepPending
			if p == nil {
				if err := s.command(ctx, command{token: "stop", stop: true}); err != nil {
					return err
				}
				break
			}
			if err := s.advanceTo(p.At); err != nil {
				return err
			}
			s.flush()
		}
	}
	var runErr error
	select {
	case runErr = <-s.runErr:
	case <-time.After(parkTimeout):
		return errors.New("engine did not finish")
	}
	s.flush()
	s.flushQuiet()
	s.summary()
	if runErr != nil && !errors.Is(runErr, contracts.ErrLeaseLost) {
		return runErr
	}
	return nil
}

func (s *sim) done() bool {
	select {
	case <-s.e.Done():
		return true
	default:
		return false
	}
}

// waitParked returns the engine's next deadline once it is blocked, or the
// zero time once Run has returned.
func (s *sim) waitParked() (time.Time, error) {
	if s.parked {
		return s.parkedAt, nil
	}
	timeout := time.After(parkTimeout)
	for {
		s.mu.Lock()
		if s.gen > s.seen {
			s.seen, s.parked, s.parkedAt = s.gen, true, s.next
			s.mu.Unlock()
			return s.parkedAt, nil
		}
		s.mu.Unlock()
		select {
		case <-s.sig:
		case <-s.e.Done():
			return time.Time{}, nil
		case <-timeout:
			return time.Time{}, errors.New("engine stalled")
		}
	}
}

func (s *sim) advanceTo(target time.Time) error {
	for {
		next, err := s.waitParked()
		if err != nil || next.IsZero() {
			return err
		}
		if next.After(target) {
			s.clock.Set(target)
			return nil
		}
		s.parked = false
		s.clock.Set(next)
		if _, err := s.waitParked(); err != nil {
			return err
		}
		s.flush()
	}
}

func (s *sim) command(ctx context.Context, c command) error {
	desc := map[string]string{
		"stop": "bckp stop", "stop:sleep": "bckp stop --sleep", "stop:nosleep": "bckp stop --no-sleep",
		"lease:lost": "daemon lease dropped", "extend:forever": "bckp extend forever",
	}[c.token]
	if c.extend && !c.forever {
		desc = fmt.Sprintf("bckp extend %d", c.minutes)
	}
	if s.done() {
		s.line(s.clock.Now(), ">", desc+" (session already ended)")
		return nil
	}
	if _, err := s.waitParked(); err != nil {
		return err
	}
	s.flushQuiet()
	s.line(s.clock.Now(), ">", desc)
	switch {
	case c.lost:
		l := s.guard.Active()
		if l == nil {
			s.line(s.clock.Now(), ">", "no active lease")
			return nil
		}
		s.parked = false
		l.Lose(errors.New("simulated: daemon connection dropped"))
	case c.stop:
		s.parked = false
		if err := s.e.Stop(ctx, c.sleep); err != nil {
			s.record(">", "refused: "+errText(err))
		}
	case c.extend:
		s.parked = false
		if _, err := s.e.Extend(ctx, c.minutes, c.forever); err != nil {
			s.record(">", "refused: "+errText(err))
		}
	}
	_, err := s.waitParked()
	return err
}

func errText(err error) string {
	if errors.Is(err, contracts.ErrNotAbortable) {
		return "not_abortable (critical sleep pending)"
	}
	return err.Error()
}

// flush prints the buffered timeline since the last flush. A poll with no
// events, the same input and the same state as the previous poll is folded
// into a "quiet" run.
func (s *sim) flush() {
	s.mu.Lock()
	items := s.items
	s.items = nil
	s.mu.Unlock()
	if len(items) == 0 {
		return
	}
	state := s.stateLabel()
	var poll *item
	events := 0
	for i := range items {
		if items[i].poll > 0 {
			poll = &items[i]
		} else {
			events++
		}
	}
	if poll != nil && events == 0 && poll.key == s.lastInput && state == s.lastState {
		if s.quiet == nil {
			s.quiet = &quietRun{from: poll.poll, at: poll.at, input: s.short(poll.poll - 1), state: state}
		}
		s.quiet.to = poll.poll
		return
	}
	s.flushQuiet()
	for _, it := range items {
		if it.poll > 0 {
			s.line(it.at, fmt.Sprintf("#%d", it.poll), fmt.Sprintf("%-34s %s", it.text, state))
			s.lastInput = it.key
			continue
		}
		if poll != nil && it.tag == "thermal" && strings.Contains(it.text, " -> ") {
			continue // the poll line shows the resulting state
		}
		s.line(it.at, it.tag, it.text)
	}
	s.lastState = state
}

func (s *sim) flushQuiet() {
	q := s.quiet
	if q == nil {
		return
	}
	s.quiet = nil
	count := q.to - q.from + 1
	if count == 1 {
		s.line(q.at, fmt.Sprintf("#%d", q.from), fmt.Sprintf("%-34s %s", q.input, q.state))
		return
	}
	s.line(q.at, fmt.Sprintf("#%d-%d", q.from, q.to), fmt.Sprintf("%-34s %s", fmt.Sprintf("%s (x%d, quiet)", q.input, count), q.state))
}

func (s *sim) stateLabel() string {
	snap := s.e.Snapshot()
	st := string(snap.Thermal.FSM)
	if p := snap.SleepPending; p != nil {
		abort := "abortable"
		if !p.Abortable {
			abort = "not abortable"
		}
		st += fmt.Sprintf(" (%s in %ds, %s)", p.Trigger, p.RemainingS, abort)
	}
	return st
}

// line writes one timeline row: relative time (only when it changed), a
// tag column, and text.
func (s *sim) line(at time.Time, tag, text string) {
	ts := ""
	if !at.Equal(s.lastTime) {
		ts = rel(at)
		s.lastTime = at
	}
	fmt.Fprintf(s.out, "  %-9s %-9s %s\n", ts, tag, text)
}

func rel(t time.Time) string {
	d := t.Sub(Start)
	return fmt.Sprintf("+%d:%02d:%02d", int(d.Hours()), int(d.Minutes())%60, int(d.Seconds())%60)
}

func (s *sim) header(script string) {
	c := s.cfg
	t := c.Thermal
	cpu := "off"
	if t.CPUSleepC != nil {
		cpu = fmt.Sprintf("%.0f°C (abortable)", *t.CPUSleepC)
	}
	session := "forever"
	if !s.p.forever {
		session = "for " + s.p.duration.String()
	}
	net := "off"
	switch {
	case c.Network.PreferHotspot == config.HotspotOn:
		net = fmt.Sprintf("prefer hotspot %q", c.Network.Service)
	case c.Network.PreferHotspot == config.HotspotAuto:
		net = fmt.Sprintf("prefer hotspot %q once connected (auto)", c.Network.Service)
	case c.Network.Watchdog:
		net = "watchdog"
	}
	if c.Network.PreferHotspot != config.HotspotOff {
		net += fmt.Sprintf("; watchdog %s", map[bool]string{true: "on", false: "off"}[c.Network.Watchdog])
	}
	nudge := ""
	if c.Network.PreferHotspot == config.HotspotOn && c.Network.WiFiNudgeAfter > 0 {
		nudge = fmt.Sprintf("\n            wifi nudge after %s offline without USB, at most every %s (lands within %s)",
			humanize.Duration(c.Network.WiFiNudgeAfter), humanize.Duration(c.Network.WiFiNudgeEvery), humanize.Duration(engine.WiFiNudgeLandWindow))
	}
	n := len(s.p.polls)
	w := s.out
	fmt.Fprintf(w, "bckp simulate: fake clock and providers; nothing is sent and nothing sleeps\n")
	fmt.Fprintf(w, "  script    %s\n", strings.Join(strings.Fields(script), " "))
	fmt.Fprintf(w, "            %s every %s (%s); session %s; clock starts %s\n",
		plural(n, "poll"), c.Session.Poll, humanize.Duration(time.Duration(n)*c.Session.Poll), session, Start.Format("15:04"))
	fmt.Fprintf(w, "  thermal   fair after %d polls (notify <= 1/%s) | hot re-notify %s | sleep after %s hot (grace %s, abortable)\n",
		t.FairPolls, humanize.Duration(t.FairCooldown), humanize.Duration(t.HotRenotify), humanize.Duration(t.SeriousSleepAfter), humanize.Duration(t.SeriousGrace))
	fmt.Fprintf(w, "            critical x%d (grace %s, not abortable) | cooled after %d | nominal after %d | cpu_sleep_c %s\n",
		t.CriticalPolls, humanize.Duration(t.CriticalGrace), t.CooledPolls, t.NominalPolls, cpu)
	fmt.Fprintf(w, "  session   cap %s (warn %s before) | heartbeat %s | battery floor %d%%\n",
		humanize.Duration(c.Session.MaxDuration), humanize.Duration(c.Session.MaxDurationWarning), humanize.Duration(c.Notify.Heartbeat), c.Power.BatteryFloor)
	fmt.Fprintf(w, "  network   %s | recover after %s offline, at most every %s%s\n\n",
		net, humanize.Duration(c.Network.RecoveryAfter), humanize.Duration(c.Network.RecoveryMinInterval), nudge)
	fmt.Fprintf(w, "  legend    #N: script poll N | pN: notification priority, 1 (min) to 5 (urgent) | guard: backpackd lease calls\n\n")
	fmt.Fprintf(w, "  %-9s %-9s %-34s %s\n", "time", "what", "detail", "thermal state after")
}

func (s *sim) summary() {
	snap := s.e.Snapshot()
	w := s.out
	s.mu.Lock()
	defer s.mu.Unlock()
	fmt.Fprintln(w)
	fmt.Fprintf(w, "  ended     %s at %s", snap.Session.EndReason, rel(s.clock.Now()))
	if snap.Session.EndReason == contracts.EndThermalSleep || snap.Session.EndReason == contracts.EndUserSleep {
		fmt.Fprint(w, " (the Mac would sleep now)")
	}
	fmt.Fprintln(w)
	if read, n := s.thermalReads, len(s.p.polls); read < n {
		which := fmt.Sprintf("#%d-%d", read+1, n)
		if read+1 == n {
			which = fmt.Sprintf("#%d", n)
		}
		fmt.Fprintf(w, "  unused    %d of %s never ran (%s)\n", n-read, plural(n, "script poll"), which)
	} else if read > len(s.p.polls) {
		fmt.Fprintf(w, "  extra     %s past the script (last inputs repeated)\n", plural(read-len(s.p.polls), "poll"))
	}
	var prios []string
	total := 0
	keys := make([]int, 0, len(s.notifyCount))
	for p := range s.notifyCount {
		keys = append(keys, p)
	}
	sort.Sort(sort.Reverse(sort.IntSlice(keys)))
	for _, p := range keys {
		prios = append(prios, fmt.Sprintf("p%d x%d", p, s.notifyCount[p]))
		total += s.notifyCount[p]
	}
	fmt.Fprintf(w, "  notified  %d: %d pushed, %d log only (notify.push = %q; %s); nothing was sent\n",
		total, s.pushed, total-s.pushed, s.cfg.Notify.Push, strings.Join(prios, ", "))
	fmt.Fprintf(w, "  guard     acquire x%d, renew x%d, sleepnow x%d, release x%d; nothing slept\n",
		s.guardCount["acquire"], s.renews, s.guardCount["sleepnow"], s.guardCount["release"])
}

// plural renders "1 poll", "2 polls".
func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// --- scripted providers -------------------------------------------------

type thermalSrc struct{ s *sim }

func (t thermalSrc) Read(ctx context.Context) (contracts.ThermalReading, error) {
	s := t.s
	s.mu.Lock()
	i := s.thermalReads
	s.thermalReads++
	s.mu.Unlock()
	in := s.input(i)
	s.mu.Lock()
	s.items = append(s.items, item{at: s.clock.Now(), poll: i + 1, key: fmt.Sprintf("%+v", in), text: s.describe(i)})
	s.mu.Unlock()
	if in.thermal.fail {
		return contracts.ThermalReading{}, errors.New("simulated thermal read failure")
	}
	return contracts.ThermalReading{State: in.thermal.state, CPUC: in.thermal.cpu, At: s.clock.Now()}, nil
}

// short renders poll i's thermal input.
func (s *sim) short(i int) string {
	in := s.input(i)
	th := in.thermal.state.String()
	if in.thermal.fail {
		th = "fail (read error)"
	}
	if in.thermal.cpu != nil {
		th += fmt.Sprintf(" %.0f°C", *in.thermal.cpu)
	}
	return th
}

// describe renders poll i's inputs: thermal always, power and network when
// they differ from the previous poll (or on the first).
func (s *sim) describe(i int) string {
	in := s.input(i)
	parts := []string{s.short(i)}
	var prev *pollInput
	if i > 0 {
		p := s.input(i - 1)
		prev = &p
	}
	if prev == nil || prev.power != in.power {
		src := "AC"
		if !in.power.OnAC {
			src = "battery"
		}
		parts = append(parts, fmt.Sprintf("%s %d%%", src, in.power.Percent))
		if prev != nil && prev.power.LidClosed != in.power.LidClosed || prev == nil && !in.power.LidClosed {
			lid := "lid open"
			if in.power.LidClosed {
				lid = "lid closed"
			}
			parts = append(parts, lid)
		}
	}
	if prev == nil || prev.net != in.net {
		kind := in.net.kind
		if in.net.unplugged {
			kind += " (phone unplugged)"
		} else if prev != nil && prev.net.unplugged {
			kind += " (phone plugged in)"
		}
		parts = append(parts, kind)
	}
	return strings.Join(parts, " | ")
}

type powerSrc struct{ s *sim }

func (p powerSrc) Read(ctx context.Context) (contracts.PowerReading, error) {
	s := p.s
	s.mu.Lock()
	i := s.powerReads
	s.powerReads++
	s.mu.Unlock()
	r := s.input(i).power
	r.At = s.clock.Now()
	return r, nil
}

// netSrc scripts Snapshot and keeps fakes.Network's model for the setters.
type netSrc struct {
	*fakes.Network
	s *sim
}

func (n *netSrc) Snapshot(ctx context.Context) (contracts.NetSnapshot, error) {
	s := n.s
	s.mu.Lock()
	i := s.nets
	s.nets++
	s.mu.Unlock()
	now := s.clock.Now()
	snap := contracts.NetSnapshot{Iface: "en10", Gateway: contracts.HotspotGateway, WiFiIface: "en0", WiFiOn: n.Network.WiFiOn(), At: now}
	in := s.input(i).net
	if in.kind == "net:fail" {
		return contracts.NetSnapshot{}, errors.New("simulated network probe failure")
	}
	if in.unplugged {
		// Wi-Fi carries the traffic, so nothing gets through while it is
		// off.
		if !snap.WiFiOn {
			return snap, nil
		}
		switch in.kind {
		case "online", "wifi:other":
			snap.DefaultIface, snap.DefaultGateway, snap.Online = "en0", "192.0.2.1", true
		case "wifi:hotspot":
			snap.DefaultIface, snap.DefaultGateway, snap.Online = "en0", contracts.HotspotGateway, true
		}
		return snap, nil
	}
	switch in.kind {
	case "online":
		snap.DefaultIface, snap.DefaultGateway, snap.Online, snap.IfacePresent = "en10", contracts.HotspotGateway, true, true
		snap.IfaceIPv4, snap.GatewayReachable, snap.IPhoneUSBPresent = "172.20.10.2", true, true
	case "offline":
		snap.IfacePresent, snap.IPhoneUSBPresent = true, true
	}
	return snap, nil
}

func (n *netSrc) SetServiceOrder(ctx context.Context, order []string) error {
	n.s.record("net", fmt.Sprintf("set service order %q", order))
	return n.Network.SetServiceOrder(ctx, order)
}

func (n *netSrc) SetServiceEnabled(ctx context.Context, svc string, on bool) error {
	state := "off"
	if on {
		state = "on"
	}
	n.s.record("net", fmt.Sprintf("service %q %s", svc, state))
	return n.Network.SetServiceEnabled(ctx, svc, on)
}

func (n *netSrc) SetWiFiPower(ctx context.Context, on bool) error {
	state := "off"
	if on {
		state = "on"
	}
	n.s.record("net", "wifi "+state)
	return n.Network.SetWiFiPower(ctx, on)
}

// guardRec records sleep-guard calls on the timeline.
type guardRec struct {
	g *fakes.SleepGuard
	s *sim
}

func (g guardRec) Acquire(ctx context.Context, ttl time.Duration, reason string) (contracts.Lease, error) {
	l, err := g.g.Acquire(ctx, ttl, reason)
	g.s.mu.Lock()
	g.s.guardCount["acquire"]++
	g.s.mu.Unlock()
	if err != nil {
		g.s.record("guard", fmt.Sprintf("acquire ttl %s: %v", humanize.Duration(ttl), err))
		return nil, err
	}
	g.s.record("guard", fmt.Sprintf("acquire ttl %s -> %s", humanize.Duration(ttl), l.ID()))
	return leaseRec{Lease: l, s: g.s}, nil
}

func (g guardRec) SleepNow(ctx context.Context, reason string) error {
	g.s.mu.Lock()
	g.s.guardCount["sleepnow"]++
	g.s.mu.Unlock()
	g.s.record("guard", fmt.Sprintf("sleepnow %q", reason))
	return g.g.SleepNow(ctx, reason)
}

func (g guardRec) Status(ctx context.Context) (contracts.GuardStatus, error) { return g.g.Status(ctx) }

type leaseRec struct {
	contracts.Lease
	s *sim
}

func (l leaseRec) Renew(ctx context.Context) error {
	l.s.mu.Lock()
	l.s.renews++
	l.s.mu.Unlock()
	return l.Lease.Renew(ctx)
}

func (l leaseRec) Release(ctx context.Context) error {
	l.s.mu.Lock()
	l.s.guardCount["release"]++
	l.s.mu.Unlock()
	l.s.record("guard", "release "+l.ID())
	return l.Lease.Release(ctx)
}
