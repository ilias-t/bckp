package daemon

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ilias-t/bckp/internal/contracts"
	"github.com/ilias-t/bckp/internal/fakes"
	"github.com/ilias-t/bckp/internal/paths"
	"github.com/ilias-t/bckp/internal/proto"
	"github.com/ilias-t/bckp/internal/providers/power"
)

func TestResetOnStartBeforeListening(t *testing.T) {
	h := newHarness(t)
	var sockExisted bool
	h.k.onFirst = func(line string) {
		_, err := os.Lstat(h.sock)
		sockExisted = err == nil
	}
	h.start()
	calls := h.k.Calls()
	if len(calls) < 2 || calls[0] != cmdOff || calls[1] != cmdIoreg {
		t.Fatalf("first calls = %v, want reset then verify", calls)
	}
	if sockExisted {
		t.Fatal("socket existed before the reset ran")
	}
	if h.k.Flag() {
		t.Fatal("flag still set after start")
	}
	st := h.status()
	if st.SleepDisabled || st.Lease != nil || st.Backstop != contracts.BackstopIdle {
		t.Fatalf("status = %+v", st)
	}
}

func TestStartRemovesStaleSocketButNotOtherFiles(t *testing.T) {
	h := newHarness(t)
	// A regular file where the socket goes: refuse (after the reset).
	if err := os.WriteFile(h.sock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	err := h.s.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "not a socket") {
		t.Fatalf("Run = %v", err)
	}
	if h.k.Flag() {
		t.Fatal("flag left set")
	}
	os.Remove(h.sock)

	// A stale socket: removed and replaced.
	h2 := newHarness(t)
	h2.sock = h.sock
	h2.s.opt.Socket = h.sock
	ln, err := listen(h.sock)
	if err != nil {
		t.Fatal(err)
	}
	ln.SetUnlinkOnClose(false)
	ln.Close()
	h2.start()
	fi, err := os.Stat(h.sock)
	if err != nil || fi.Mode().Perm() != 0o666 {
		t.Fatalf("socket mode = %v, %v", fi.Mode(), err)
	}
}

func TestResetFailureOnStartIsFatal(t *testing.T) {
	h := newHarness(t)
	h.k.stuck = true
	err := h.s.Run(context.Background())
	if err == nil {
		t.Fatal("Run succeeded with a stuck flag")
	}
	if n := strings.Count(joinCalls(h.k.Calls()), cmdOff); n != 2 {
		t.Fatalf("reset attempts = %d, want 2 (one retry)", n)
	}
	if _, err := os.Lstat(h.sock); err == nil {
		t.Fatal("listened despite failed reset")
	}
}

func TestSetFlagVerifiesAndRetriesOnce(t *testing.T) {
	ex := fakes.NewExec().
		On(cmdOff, "").
		On(cmdIoreg, ioregYes).On(cmdIoreg, ioregNo)
	s := New(Deps{Exec: ex, Clock: fakes.NewClock(start), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}, Options{})
	if err := s.setFlag(false); err != nil {
		t.Fatalf("setFlag: %v", err)
	}
	want := []string{cmdOff, cmdIoreg, cmdOff, cmdIoreg}
	if got := ex.Calls(); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("calls = %v, want %v", got, want)
	}

	// ioreg output without the key never verifies.
	ex = fakes.NewExec().On(cmdOn, "").On(cmdIoreg, "garbage")
	s.d.Exec = ex
	if err := s.setFlag(true); err == nil {
		t.Fatal("verified without SleepDisabled in ioreg output")
	}
	if n := ex.CallCount(cmdOn); n != 2 {
		t.Fatalf("pmset attempts = %d, want 2", n)
	}
}

func TestAcquireRenewExpiry(t *testing.T) {
	h := newHarness(t)
	h.start()
	c := h.dial()
	id := c.acquire(30)
	if !h.k.Flag() {
		t.Fatal("flag not set after acquire")
	}
	h.waitTimers(2)

	h.clock.Advance(20 * time.Second) // poll at +10s fired; re-armed
	h.waitTimers(2)
	m := c.do(fmt.Sprintf(`{"op":"renew","lease":%q}`, id))
	if m["ok"] != true || m["expires"] != start.Add(50*time.Second).Format(time.RFC3339) {
		t.Fatalf("renew = %v", m)
	}
	h.clock.Advance(15 * time.Second) // +35: past the original expiry, before the renewed one
	h.waitTimers(2)
	if st := h.status(); st.Lease == nil || st.Lease.ID != id || !h.k.Flag() {
		t.Fatalf("lease gone before renewed expiry: %+v", st)
	}
	h.clock.Advance(15 * time.Second) // +50
	h.eventually("expiry reset", func() bool { return !h.k.Flag() })
	if !c.closed() {
		t.Fatal("lease connection not closed on expiry")
	}
	if st := h.status(); st.Lease != nil || st.SleepDisabled || st.Backstop != contracts.BackstopIdle {
		t.Fatalf("status after expiry = %+v", st)
	}
	if h.k.Sleeps() != 0 {
		t.Fatal("expiry must not sleep the Mac")
	}
}

func TestTTLClamped(t *testing.T) {
	h := newHarness(t)
	h.start()
	c := h.dial()
	c.acquire(5)
	if st := h.status(); st.Lease.TTLS != 30 {
		t.Fatalf("ttl = %d, want 30", st.Lease.TTLS)
	}
	c.do(fmt.Sprintf(`{"op":"release","lease":%q}`, h.status().Lease.ID))
	c.acquire(100000)
	if st := h.status(); st.Lease.TTLS != 300 {
		t.Fatalf("ttl = %d, want 300", st.Lease.TTLS)
	}
}

func TestReasonTruncatedAndSanitized(t *testing.T) {
	h := newHarness(t)
	h.start()
	c := h.dial()
	c.do(fmt.Sprintf(`{"op":"acquire","reason":"a\nb%s"}`, strings.Repeat("x", 500)))
	r := h.status().Lease.Reason
	if len([]rune(r)) != proto.MaxReasonLen || strings.ContainsAny(r, "\n") {
		t.Fatalf("reason = %q (%d runes)", r, len([]rune(r)))
	}
}

func TestReleaseKeepsConnectionOpen(t *testing.T) {
	h := newHarness(t)
	h.start()
	c := h.dial()
	id := c.acquire(90)
	if m := c.do(fmt.Sprintf(`{"op":"release","lease":%q}`, id)); m["ok"] != true {
		t.Fatalf("release = %v", m)
	}
	if h.k.Flag() {
		t.Fatal("flag set after release")
	}
	wantCode(t, c.do(fmt.Sprintf(`{"op":"renew","lease":%q}`, id)), proto.CodeNoLease)
	c.acquire(90) // same connection can acquire again
}

func TestConnectionCloseResets(t *testing.T) {
	h := newHarness(t)
	h.start()
	c := h.dial()
	c.acquire(300)
	c.c.Close()
	h.eventually("reset after close", func() bool { return !h.k.Flag() })
	if st := h.status(); st.Lease != nil {
		t.Fatalf("lease survived connection close: %+v", st.Lease)
	}
	calls := h.k.Calls()
	if calls[len(calls)-2] != cmdOff || calls[len(calls)-1] != cmdIoreg {
		t.Fatalf("calls = %v", calls)
	}
}

func TestSecondAcquireBusyAndMismatch(t *testing.T) {
	h := newHarness(t)
	h.start()
	a, b := h.dial(), h.dial()
	id := a.acquire(90)
	wantCode(t, b.do(`{"op":"acquire"}`), proto.CodeBusy)
	wantCode(t, a.do(`{"op":"acquire"}`), proto.CodeBusy)
	wantCode(t, a.do(`{"op":"renew","lease":"nope"}`), proto.CodeLeaseMismatch)
	wantCode(t, b.do(fmt.Sprintf(`{"op":"renew","lease":%q}`, id)), proto.CodeLeaseMismatch)
	wantCode(t, b.do(fmt.Sprintf(`{"op":"release","lease":%q}`, id)), proto.CodeLeaseMismatch)
	wantCode(t, b.do(`{"op":"sleep"}`), proto.CodeBusy)
	if !h.k.Flag() || h.k.Sleeps() != 0 {
		t.Fatal("another connection disturbed the lease")
	}
}

func TestAllowlistDeny(t *testing.T) {
	uid := os.Getuid()
	cases := map[string]func(h *harness){
		"uid not listed": func(h *harness) { h.writeAllow(fmt.Sprintf("%d\n", uid+1), 0o644) },
		"missing":        func(h *harness) { os.Remove(h.allow) },
		"world writable": func(h *harness) { h.writeAllow(fmt.Sprintf("%d\n", uid), 0o666) },
		"group writable": func(h *harness) { h.writeAllow(fmt.Sprintf("%d\n", uid), 0o664) },
		"unreadable":     func(h *harness) { h.writeAllow(fmt.Sprintf("%d\n", uid), 0o000) },
		"malformed":      func(h *harness) { h.writeAllow(fmt.Sprintf("%d # me\n", uid), 0o644) },
		"wrong owner":    func(h *harness) { h.s.opt.AllowlistOwner = uid + 1 },
		"symlink": func(h *harness) {
			real := filepath.Join(h.dir, "real")
			os.WriteFile(real, []byte(fmt.Sprintf("%d\n", uid)), 0o644)
			os.Remove(h.allow)
			os.Symlink(real, h.allow)
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			setup(h)
			h.start()
			c := h.dial()
			c.send(`{"op":"acquire"}`)
			wantCode(t, c.recv(), proto.CodeNotAllowed)
			if !c.closed() {
				t.Fatal("denied connection left open")
			}
			if h.k.Flag() {
				t.Fatal("denied caller set the flag")
			}
		})
	}
}

func TestParseAllowlist(t *testing.T) {
	good := map[string][]uint32{"501\n": {501}, "501": {501}, "0\n501\n502\n": {0, 501, 502}}
	for in, want := range good {
		m, err := parseAllowlist(in)
		if err != nil || len(m) != len(want) {
			t.Errorf("%q: %v %v", in, m, err)
		}
		for _, u := range want {
			if !m[u] {
				t.Errorf("%q: missing %d", in, u)
			}
		}
	}
	for _, in := range []string{"", "\n", "501\n\n", " 501", "501 ", "-1", "+5", "abc", "501\r\n", "4294967296", "501\n#x"} {
		if m, err := parseAllowlist(in); err == nil {
			t.Errorf("%q accepted: %v", in, m)
		}
	}
}

func TestOversizedRequest(t *testing.T) {
	h := newHarness(t)
	h.start()
	c := h.dial()
	c.send(`{"op":"acquire","reason":"` + strings.Repeat("x", proto.MaxDaemonRequestBytes) + `"}`)
	wantCode(t, c.recv(), proto.CodeTooLarge)
	if !c.closed() {
		t.Fatal("connection left open after oversized request")
	}
	if h.k.Flag() {
		t.Fatal("oversized request set the flag")
	}
}

func TestBadRequests(t *testing.T) {
	h := newHarness(t)
	h.start()
	c := h.dial()
	// Validation failures keep the connection open.
	wantCode(t, c.do(`{"op":"frobnicate"}`), proto.CodeBadRequest)
	wantCode(t, c.do(`{"op":"status","lease":"x"}`), proto.CodeBadRequest)
	wantCode(t, c.do(`{"op":"renew"}`), proto.CodeBadRequest)
	wantCode(t, c.do(`{"op":"renew","lease":"x"}`), proto.CodeNoLease)
	if m := c.do(`{"op":"status"}`); m["ok"] != true {
		t.Fatalf("status after validation errors = %v", m)
	}
	// Malformed JSON and unknown fields close the connection.
	for _, line := range []string{`{"op":`, `{"op":"status","cmd":"rm -rf /"}`, `{"op":"status"} {}`} {
		c := h.dial()
		wantCode(t, c.do(line), proto.CodeBadRequest)
		if !c.closed() {
			t.Fatalf("%s: connection left open", line)
		}
	}
	for _, call := range h.k.Calls() {
		if call != cmdOff && call != cmdIoreg {
			t.Fatalf("unexpected command %q", call)
		}
	}
}

func TestSleepOpOrdering(t *testing.T) {
	h := newHarness(t)
	h.start()
	c := h.dial()
	id := c.acquire(90)
	answered := make(chan struct{})
	var flagAtSleep bool
	h.k.onSleep = func() {
		select {
		case <-answered:
		case <-time.After(waitBudget):
			t.Error("sleepnow ran before the client got its answer")
		}
		flagAtSleep = h.k.Flag()
	}
	m := c.do(fmt.Sprintf(`{"op":"sleep","lease":%q,"reason":"thermal critical"}`, id))
	close(answered)
	if m["ok"] != true {
		t.Fatalf("sleep = %v", m)
	}
	h.eventually("sleepnow", func() bool { return h.k.Sleeps() == 1 })
	if flagAtSleep {
		t.Fatal("sleepnow ran with disablesleep 1")
	}
	calls := h.k.Calls()
	if got := calls[len(calls)-3:]; strings.Join(got, "|") != strings.Join([]string{cmdOff, cmdIoreg, cmdSleep}, "|") {
		t.Fatalf("tail calls = %v", got)
	}
	if st := h.status(); st.Lease != nil {
		t.Fatal("lease survived sleep")
	}
}

func TestSleepWithoutLease(t *testing.T) {
	h := newHarness(t)
	h.start()
	c := h.dial()
	wantCode(t, c.do(`{"op":"sleep","lease":"stale"}`), proto.CodeNoLease)
	if m := c.do(`{"op":"sleep","reason":"cli"}`); m["ok"] != true {
		t.Fatalf("sleep = %v", m)
	}
	h.eventually("sleepnow", func() bool { return h.k.Sleeps() == 1 })
}

func TestSleepVerifyFailureDoesNotSleep(t *testing.T) {
	h := newHarness(t)
	h.start()
	c := h.dial()
	id := c.acquire(90)
	h.k.mu.Lock()
	h.k.stuck = true
	h.k.mu.Unlock()
	wantCode(t, c.do(fmt.Sprintf(`{"op":"sleep","lease":%q}`, id)), proto.CodeInternal)
	time.Sleep(20 * time.Millisecond)
	if h.k.Sleeps() != 0 {
		t.Fatal("sleepnow ran although disablesleep 0 did not verify")
	}
}

// backstopCase acquires a lease, feeds readings, and advances poll by poll.
func runBackstop(t *testing.T, thermal []contracts.ThermalState, thermalErrAt int, power []contracts.PowerReading) (*harness, *client) {
	h := newHarness(t)
	h.thermal = fakes.NewThermal(h.clock)
	for i, s := range thermal {
		if i == thermalErrAt {
			h.thermal.PushErr(errors.New("shim failed"))
		}
		h.thermal.Push(contracts.ThermalReading{State: s})
	}
	h.power = fakes.NewPower(h.clock, power...)
	h.s.d.Thermal, h.s.d.Power = h.thermal, h.power
	h.start()
	c := h.dial()
	c.acquire(300)
	return h, c
}

func (h *harness) poll() {
	h.t.Helper()
	h.waitTimers(2)
	h.clock.Advance(proto.BackstopPollInterval)
}

func TestBackstopThermalCriticalTwice(t *testing.T) {
	crit, nom := contracts.ThermalCritical, contracts.ThermalNominal
	h, c := runBackstop(t, []contracts.ThermalState{nom, crit, crit}, -1, []contracts.PowerReading{{OnAC: true, Percent: 50}})
	h.poll()
	h.poll()
	h.waitTimers(2)
	if !h.k.Flag() || h.k.Sleeps() != 0 {
		t.Fatal("tripped after one critical reading")
	}
	h.poll()
	h.eventually("sleepnow", func() bool { return h.k.Sleeps() == 1 })
	if h.k.Flag() {
		t.Fatal("flag set after backstop")
	}
	if !c.closed() {
		t.Fatal("lease connection left open after backstop")
	}
	st := h.status()
	if st.Lease != nil || st.Backstop != contracts.BackstopTrippedPrefix+"thermal critical x2" {
		t.Fatalf("status = %+v", st)
	}
	calls := h.k.Calls()
	if !hasSubseq(calls[len(calls)-3:], cmdOff, cmdIoreg, cmdSleep) {
		t.Fatalf("calls = %v", calls)
	}
	// The trip is reported until the next acquire.
	c2 := h.dial()
	c2.acquire(90)
	if st := h.status(); st.Backstop != contracts.BackstopArmed {
		t.Fatalf("backstop after re-acquire = %q", st.Backstop)
	}
}

func TestBackstopThermalNotConsecutiveDoesNotTrip(t *testing.T) {
	crit, nom := contracts.ThermalCritical, contracts.ThermalNominal
	h, _ := runBackstop(t, []contracts.ThermalState{crit, nom, crit, nom}, -1, []contracts.PowerReading{{OnAC: false, Percent: 50}})
	for range 4 {
		h.poll()
	}
	h.waitTimers(2)
	if h.k.Sleeps() != 0 || !h.k.Flag() {
		t.Fatal("tripped on non-consecutive critical readings")
	}
	if st := h.status(); st.Backstop != contracts.BackstopArmed {
		t.Fatalf("backstop = %q", st.Backstop)
	}
}

// A read error or unknown state between critical readings is skipped: it
// neither trips nor resets the count (conservative: the Mac is hot).
func TestBackstopThermalErrorDoesNotResetCount(t *testing.T) {
	crit, unknown := contracts.ThermalCritical, contracts.ThermalUnknown
	h, _ := runBackstop(t, []contracts.ThermalState{crit, unknown, crit}, 1, []contracts.PowerReading{{OnAC: true, Percent: 50}})
	h.poll() // critical x1
	h.poll() // error
	h.poll() // unknown
	h.waitTimers(2)
	if h.k.Sleeps() != 0 {
		t.Fatal("a read error or unknown state tripped the backstop")
	}
	h.poll() // critical x2
	h.eventually("sleepnow", func() bool { return h.k.Sleeps() == 1 })
	if st := h.status(); st.Backstop != contracts.BackstopTrippedPrefix+"thermal critical x2" {
		t.Fatalf("backstop = %q", st.Backstop)
	}
}

func TestBackstopThermalErrorsDoNotTrip(t *testing.T) {
	h := newHarness(t)
	h.thermal = fakes.NewThermal(h.clock)
	h.thermal.PushErr(errors.New("shim failed"))
	h.s.d.Thermal = h.thermal
	h.start()
	c := h.dial()
	c.acquire(300)
	for range 5 {
		h.poll()
	}
	h.waitTimers(2)
	if h.k.Sleeps() != 0 || !h.k.Flag() {
		t.Fatal("thermal read errors tripped the backstop")
	}
}

func TestBackstopBattery(t *testing.T) {
	nom := []contracts.ThermalState{contracts.ThermalNominal}
	t.Run("low on battery trips", func(t *testing.T) {
		h, c := runBackstop(t, nom, -1, []contracts.PowerReading{{OnAC: false, Percent: 9}})
		h.poll()
		h.eventually("sleepnow", func() bool { return h.k.Sleeps() == 1 })
		if h.k.Flag() || !c.closed() {
			t.Fatal("battery backstop did not release")
		}
		if st := h.status(); st.Backstop != contracts.BackstopTrippedPrefix+"battery 9%" {
			t.Fatalf("backstop = %q", st.Backstop)
		}
	})
	t.Run("low on AC does not trip", func(t *testing.T) {
		h, _ := runBackstop(t, nom, -1, []contracts.PowerReading{{OnAC: true, Percent: 3}})
		for range 3 {
			h.poll()
		}
		h.waitTimers(2)
		if h.k.Sleeps() != 0 || !h.k.Flag() {
			t.Fatal("tripped while on AC")
		}
	})
	t.Run("ten percent does not trip", func(t *testing.T) {
		h, _ := runBackstop(t, nom, -1, []contracts.PowerReading{{OnAC: false, Percent: 10}})
		h.poll()
		h.waitTimers(2)
		if h.k.Sleeps() != 0 {
			t.Fatal("tripped at 10%")
		}
	})
	t.Run("power read error does not trip", func(t *testing.T) {
		h := newHarness(t)
		h.power = fakes.NewPower(h.clock)
		h.power.PushErr(errors.New("pmset -g batt failed"))
		h.s.d.Power = h.power
		h.start()
		h.dial().acquire(300)
		h.poll()
		h.waitTimers(2)
		if h.k.Sleeps() != 0 {
			t.Fatal("power read error tripped")
		}
	})
}

func TestBackstopIdleWithoutLease(t *testing.T) {
	h := newHarness(t)
	h.start()
	time.Sleep(10 * time.Millisecond)
	if n := h.clock.PendingTimers(); n != 0 {
		t.Fatalf("%d timers pending without a lease", n)
	}
	if h.thermal.Reads() != 0 {
		t.Fatal("polled without a lease")
	}
}

func TestConcurrentStatusWhileLeaseHeld(t *testing.T) {
	h := newHarness(t)
	h.start()
	c := h.dial()
	id := c.acquire(90)
	var wg sync.WaitGroup
	errs := make(chan string, 20)
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				st, err := h.tryStatus()
				if err != nil {
					errs <- err.Error()
					return
				}
				if st.OK { // 20 callers > 8 slots: busy answers are retried
					if st.Lease == nil || st.Lease.ID != id || !st.SleepDisabled || st.Backstop != contracts.BackstopArmed {
						errs <- fmt.Sprintf("status = %+v", st)
					}
					return
				}
				if st.Code != proto.CodeBusy {
					errs <- fmt.Sprintf("status failed: %+v", st)
					return
				}
				time.Sleep(time.Millisecond)
			}
		}()
	}
	// Renew concurrently on the lease connection.
	for range 5 {
		if m := c.do(fmt.Sprintf(`{"op":"renew","lease":%q}`, id)); m["ok"] != true {
			t.Fatalf("renew = %v", m)
		}
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
}

func TestConnectionLimit(t *testing.T) {
	h := newHarness(t)
	h.start()
	var held []*client
	for range proto.MaxDaemonConns {
		c := h.dial()
		c.do(`{"op":"status"}`) // make sure it is registered
		held = append(held, c)
	}
	extra := h.dial()
	wantCode(t, extra.recv(), proto.CodeBusy)
	if !extra.closed() {
		t.Fatal("extra connection left open")
	}
	held[0].c.Close()
	h.eventually("slot freed", func() bool {
		c := h.dial()
		c.send(`{"op":"status"}`)
		m := c.recv()
		c.c.Close()
		return m["ok"] == true
	})
}

func TestShutdownResetsAndClosesLease(t *testing.T) {
	h := newHarness(t)
	h.start()
	c := h.dial()
	c.acquire(90)
	h.stop()
	if h.k.Flag() {
		t.Fatal("flag set after shutdown")
	}
	if !c.closed() {
		t.Fatal("lease connection open after shutdown")
	}
	if _, err := os.Lstat(h.sock); err == nil {
		t.Fatal("socket left behind")
	}
}

func TestMainRefusesWithoutSources(t *testing.T) {
	ex := fakes.NewExec().On(cmdOff, "").On(cmdIoreg, ioregNo)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if code := Main(Deps{Exec: ex, Clock: fakes.NewClock(start), Log: log}); code != 2 {
		t.Fatalf("Main = %d, want 2", code)
	}
	if got := ex.Calls(); len(got) != 2 || got[0] != cmdOff {
		t.Fatalf("calls = %v, want a reset", got)
	}
	if code := Main(Deps{Log: log}); code != 2 {
		t.Fatalf("Main without Exec = %d", code)
	}
}

func TestLaunchdPlist(t *testing.T) {
	plist := LaunchdPlist()
	var doc struct {
		Dict struct {
			Inner []byte `xml:",innerxml"`
		} `xml:"dict"`
	}
	if err := xml.Unmarshal(plist, &doc); err != nil {
		t.Fatalf("plist is not XML: %v", err)
	}
	for _, want := range []string{
		"<key>Label</key>\n\t<string>" + paths.DaemonLabel + "</string>",
		"<string>" + paths.DaemonBinary + "</string>",
		"<key>RunAtLoad</key>\n\t<true/>",
		"<key>KeepAlive</key>\n\t<true/>",
		"<key>ExitTimeOut</key>\n\t<integer>60</integer>",
		"/Library/Logs/backpack/",
	} {
		if !strings.Contains(string(plist), want) {
			t.Errorf("plist missing %q", want)
		}
	}
	LaunchdPlist()[0] = 'X'
	if LaunchdPlist()[0] == 'X' {
		t.Fatal("LaunchdPlist exposes the embedded bytes")
	}
}

// The daemon keeps its ioreg argv as a fixed array; it must stay the one
// power.ParseClamshell is written for.
func TestReadFlagArgvMatchesPowerProvider(t *testing.T) {
	if !slices.Equal(argvReadFlag[:], power.ClamshellArgs) {
		t.Fatalf("argvReadFlag = %v, power.ClamshellArgs = %v", argvReadFlag, power.ClamshellArgs)
	}
}

func TestIdleCheckResetsFlagWithoutLease(t *testing.T) {
	h := newHarness(t).withIdleCheck(time.Minute)
	s := logged(h)
	h.start()

	// Set by hand (or left by a reset that did not verify): reset within
	// one idle check.
	h.k.Set(true)
	h.waitTimers(1)
	h.clock.Advance(time.Minute)
	h.eventually("idle reset", func() bool { return !h.k.Flag() })
	h.eventually("reconcile event", func() bool { return hasEvent(s, "flag.reconcile", "result", "reset") })

	// A reset that does not verify is retried at every check until it
	// does.
	h.k.SetStuck(true)
	h.k.Set(true)
	h.waitTimers(1)
	h.clock.Advance(time.Minute)
	h.eventually("failed reconcile", func() bool { return hasEvent(s, "flag.reconcile", "result", "failed") })
	if !h.k.Flag() {
		t.Fatal("stuck flag cleared")
	}
	h.k.SetStuck(false)
	h.waitTimers(1)
	h.clock.Advance(time.Minute)
	h.eventually("retried reset", func() bool { return !h.k.Flag() })
	if st := h.status(); st.SleepDisabled {
		t.Fatalf("status = %+v", st)
	}
}

func TestIdleCheckLeavesLeaseAlone(t *testing.T) {
	h := newHarness(t).withIdleCheck(time.Minute)
	s := logged(h)
	h.start()
	c := h.dial()
	id := c.acquire(90)
	h.waitTimers(3) // idle check + lease expiry + backstop poll
	h.clock.Advance(time.Minute)
	h.waitTimers(3)
	if !h.k.Flag() {
		t.Fatal("idle check reset the flag under a lease")
	}
	if hasEvent(s, "flag.reconcile") {
		t.Fatal("flag.reconcile logged while a lease was held")
	}
	c.do(fmt.Sprintf(`{"op":"release","lease":%q}`, id))
}
