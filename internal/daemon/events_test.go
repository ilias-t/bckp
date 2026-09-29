package daemon

import (
	"bytes"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ilias-t/bckp/internal/contracts"
	"github.com/ilias-t/bckp/internal/eventlog"
	"github.com/ilias-t/bckp/internal/fakes"
)

type sink struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *sink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *sink) events(t *testing.T) []eventlog.Event {
	t.Helper()
	s.mu.Lock()
	raw := s.buf.String()
	s.mu.Unlock()
	var out []eventlog.Event
	for _, l := range strings.Split(strings.TrimSpace(raw), "\n") {
		if l == "" {
			continue
		}
		ev, err := eventlog.ParseLine([]byte(l))
		if err != nil {
			t.Fatalf("bad line %q: %v", l, err)
		}
		out = append(out, ev)
	}
	return out
}

func (s *sink) find(t *testing.T, name string, kv ...string) eventlog.Event {
	t.Helper()
	var names []string
	for _, ev := range s.events(t) {
		names = append(names, ev.Name)
		if ev.Name != name {
			continue
		}
		ok := true
		for i := 0; i+1 < len(kv); i += 2 {
			if v, _ := ev.Get(kv[i]); fmt.Sprint(v) != kv[i+1] {
				ok = false
			}
		}
		if ok {
			return ev
		}
	}
	t.Fatalf("no %s %v event; got %v", name, kv, names)
	return eventlog.Event{}
}

// hasEvent reports whether a name event with the given fields was logged.
func hasEvent(s *sink, name string, kv ...string) bool {
	s.mu.Lock()
	raw := s.buf.String()
	s.mu.Unlock()
	for _, l := range strings.Split(strings.TrimSpace(raw), "\n") {
		ev, err := eventlog.ParseLine([]byte(l))
		if err != nil || ev.Name != name {
			continue
		}
		ok := true
		for i := 0; i+1 < len(kv); i += 2 {
			if v, _ := ev.Get(kv[i]); fmt.Sprint(v) != kv[i+1] {
				ok = false
			}
		}
		if ok {
			return true
		}
	}
	return false
}

func (s *sink) count(t *testing.T, name string) int {
	t.Helper()
	n := 0
	for _, ev := range s.events(t) {
		if ev.Name == name {
			n++
		}
	}
	return n
}

// logged attaches an eventlog JSON handler to the harness's server.
func logged(h *harness) *sink {
	s := &sink{}
	h.s.log = slog.New(eventlog.NewHandler(s, eventlog.Options{})).With("src", eventlog.SrcDaemon)
	return s
}

func TestDaemonEventsLeaseLifecycle(t *testing.T) {
	h := newHarness(t)
	s := logged(h)
	h.start()
	c := h.dial()
	id := c.acquire(30)
	c.do(fmt.Sprintf(`{"op":"renew","lease":%q}`, id))
	c.do(fmt.Sprintf(`{"op":"release","lease":%q}`, id))
	c2 := h.dial()
	id2 := c2.acquire(30)
	c2.c.Close()
	h.eventually("reset after close", func() bool { return !h.k.Flag() })
	c3 := h.dial()
	id3 := c3.acquire(30)
	h.waitTimers(2)
	h.clock.Advance(31 * time.Second)
	h.eventually("expiry", func() bool { return !h.k.Flag() })
	h.stop()

	start := s.find(t, "daemon.start")
	if start.Src != eventlog.SrcDaemon {
		t.Fatalf("src = %q", start.Src)
	}
	s.find(t, "flag.set", "value", "false", "verified", "true")
	s.find(t, "flag.set", "value", "true", "verified", "true")
	acq := s.find(t, "lease.acquire", "lease", id, "reason", "test")
	if v, _ := acq.Get("uid"); fmt.Sprint(v) != fmt.Sprint(os.Getuid()) {
		t.Fatalf("lease.acquire uid = %v", v)
	}
	s.find(t, "lease.renew", "lease", id)
	s.find(t, "lease.release", "lease", id)
	if ev := s.find(t, "lease.conn_closed", "lease", id2); ev.Level != slog.LevelWarn {
		t.Fatalf("conn_closed level %v", ev.Level)
	}
	s.find(t, "lease.expire", "lease", id3)
	s.find(t, "daemon.stop", "reason", "signal")
}

func TestDaemonEventsBackstopAndSleep(t *testing.T) {
	crit := contracts.ThermalCritical
	h := newHarness(t)
	s := logged(h)
	h.thermal = fakes.NewThermal(h.clock, contracts.ThermalReading{State: crit})
	h.s.d.Thermal = h.thermal
	h.start()
	c := h.dial()
	id := c.acquire(300)
	h.poll()
	h.poll()
	h.eventually("sleepnow", func() bool { return h.k.Sleeps() == 1 })
	h.stop()
	s.find(t, "backstop.trip", "reason", "thermal critical x2", "lease", id)
	s.find(t, "lease.end", "lease", id, "why", "backstop: thermal critical x2")
	s.find(t, "sleep.now", "result", "ok")

	h2 := newHarness(t)
	s2 := logged(h2)
	h2.start()
	c2 := h2.dial()
	id2 := c2.acquire(90)
	c2.do(fmt.Sprintf(`{"op":"sleep","lease":%q,"reason":"thermal critical"}`, id2))
	h2.eventually("sleepnow", func() bool { return h2.k.Sleeps() == 1 })
	h2.stop()
	s2.find(t, "sleep.op", "reason", "thermal critical", "result", "ok")
	s2.find(t, "lease.end", "why", "sleep requested")
}

func TestDaemonEventsAuthDeny(t *testing.T) {
	h := newHarness(t)
	s := logged(h)
	h.writeAllow(fmt.Sprintf("%d\n", os.Getuid()+1), 0o644)
	h.start()
	c := h.dial()
	c.send(`{"op":"acquire"}`)
	c.recv()
	h.stop()
	ev := s.find(t, "auth.deny", "uid", fmt.Sprint(os.Getuid()))
	if ev.Level != slog.LevelWarn {
		t.Fatalf("auth.deny level %v", ev.Level)
	}
}

func TestMainLogsToLogDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "Logs", "backpack")
	ex := fakes.NewExec().On(cmdOff, "").On(cmdIoreg, ioregNo)
	// No backstop sources: Main resets the flag and refuses to serve, which
	// is enough to see where it logs.
	if code := Main(Deps{Exec: ex, Clock: fakes.NewClock(start), LogDir: dir}); code != 2 {
		t.Fatalf("Main = %d", code)
	}
	di, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(dir, "backpackd.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if di.Mode().Perm() != 0o755 || fi.Mode().Perm() != 0o644 {
		t.Fatalf("modes %v / %v, want 0755 / 0644", di.Mode().Perm(), fi.Mode().Perm())
	}
	evs, _, err := eventlog.ReadDirs([]string{dir}, eventlog.Filter{MinLevel: slog.LevelDebug})
	if err != nil || len(evs) == 0 {
		t.Fatalf("events %v, err %v", evs, err)
	}
	var names []string
	for _, ev := range evs {
		names = append(names, ev.Name)
		if ev.Src != eventlog.SrcDaemon {
			t.Fatalf("src = %q", ev.Src)
		}
	}
	if !strings.Contains(strings.Join(names, " "), "flag.set") {
		t.Fatalf("events = %v", names)
	}

	// An unusable dir falls back to stderr and still resets the flag.
	blocker := filepath.Join(t.TempDir(), "file")
	_ = os.WriteFile(blocker, nil, 0o600)
	ex2 := fakes.NewExec().On(cmdOff, "").On(cmdIoreg, ioregNo)
	if code := Main(Deps{Exec: ex2, Clock: fakes.NewClock(start), LogDir: filepath.Join(blocker, "sub")}); code != 2 {
		t.Fatalf("Main with bad LogDir = %d", code)
	}
	if calls := ex2.Calls(); len(calls) == 0 || calls[0] != cmdOff {
		t.Fatalf("calls = %v; the reset must still happen", calls)
	}

	// A symlinked log file is not followed: stderr instead, target intact.
	linkDir := t.TempDir()
	target := filepath.Join(t.TempDir(), "target")
	_ = os.WriteFile(target, []byte("keep\n"), 0o600)
	if err := os.Symlink(target, filepath.Join(linkDir, "backpackd.jsonl")); err != nil {
		t.Fatal(err)
	}
	ex3 := fakes.NewExec().On(cmdOff, "").On(cmdIoreg, ioregNo)
	if code := Main(Deps{Exec: ex3, Clock: fakes.NewClock(start), LogDir: linkDir}); code != 2 {
		t.Fatalf("Main with symlinked log = %d", code)
	}
	if calls := ex3.Calls(); len(calls) == 0 || calls[0] != cmdOff {
		t.Fatalf("calls = %v; the reset must still happen", calls)
	}
	if b, _ := os.ReadFile(target); string(b) != "keep\n" {
		t.Fatalf("wrote through the symlink: %q", b)
	}
	if fi, _ := os.Stat(target); fi.Mode().Perm() != 0o600 {
		t.Fatalf("chmodded through the symlink: %v", fi.Mode())
	}
}

func TestDaemonEventsAuthDenyRateLimited(t *testing.T) {
	h := newHarness(t)
	s := logged(h)
	h.start()
	// Deny from here on, once start's own probe connection is served: a
	// status call is accepted after it, then both are dropped.
	h.status()
	h.eventually("probe connection dropped", func() bool {
		h.s.mu.Lock()
		defer h.s.mu.Unlock()
		return len(h.s.conns) == 0
	})
	h.writeAllow(fmt.Sprintf("%d\n", os.Getuid()+1), 0o644)
	deny := func(n int) {
		for range n {
			c := h.dial()
			c.send(`{"op":"status"}`)
			c.recv()
			c.c.Close()
		}
	}
	deny(logBurst + 5)
	if n := s.count(t, "auth.deny"); n != logBurst {
		t.Fatalf("auth.deny logged %d times, want %d", n, logBurst)
	}
	if s.count(t, "log.suppressed") != 0 {
		t.Fatal("summary before the window ended")
	}

	// The next deny after the window reports the suppressed ones first.
	h.clock.Advance(logWindow)
	deny(1)
	s.find(t, "log.suppressed", "suppressed_event", "auth.deny", "count", "5", "logged", fmt.Sprint(logBurst))
	if n := s.count(t, "auth.deny"); n != logBurst+1 {
		t.Fatalf("auth.deny logged %d times, want %d", n, logBurst+1)
	}

	// A burst with nothing after it is summarised at shutdown.
	deny(logBurst + 2)
	h.stop()
	s.find(t, "log.suppressed", "suppressed_event", "auth.deny", "count", "3")
}
