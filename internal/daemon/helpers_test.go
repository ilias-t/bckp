package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
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
)

// Command lines backpackd may run.
var (
	cmdOff     = contracts.CommandLine(paths.BinPmset, argvSleepOff[:]...)
	cmdOn      = contracts.CommandLine(paths.BinPmset, argvSleepOn[:]...)
	cmdSleep   = contracts.CommandLine(paths.BinPmset, argvSleepNow[:]...)
	cmdIoreg   = contracts.CommandLine(paths.BinIoreg, argvReadFlag[:]...)
	ioregYes   = "+-o IOPMrootDomain\n  |   \"AppleClamshellState\" = Yes\n  |   \"SleepDisabled\" = Yes\n"
	ioregNo    = "+-o IOPMrootDomain\n  |   \"AppleClamshellState\" = Yes\n  |   \"SleepDisabled\" = No\n"
	start      = time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	waitBudget = 3 * time.Second
)

// kernel is a stateful fake Exec modeling the SleepDisabled flag: pmset
// sets it, ioreg reports it. Unknown commands fail.
type kernel struct {
	mu       sync.Mutex
	flag     bool
	calls    []string
	stuck    bool         // pmset -a disablesleep 0 has no effect
	onSleep  func()       // runs (unlocked) before sleepnow is recorded
	onFirst  func(string) // runs on the first call
	sleepCnt int
}

func (k *kernel) Run(ctx context.Context, name string, args ...string) (contracts.ExecResult, error) {
	line := contracts.CommandLine(name, args...)
	k.mu.Lock()
	first, hook := len(k.calls) == 0, k.onSleep
	onFirst := k.onFirst
	k.mu.Unlock()
	if first && onFirst != nil {
		onFirst(line)
	}
	if line == cmdSleep && hook != nil {
		hook()
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	k.calls = append(k.calls, line)
	switch line {
	case cmdOn:
		k.flag = true
	case cmdOff:
		if !k.stuck {
			k.flag = false
		}
	case cmdSleep:
		k.sleepCnt++
	case cmdIoreg:
		out := ioregNo
		if k.flag {
			out = ioregYes
		}
		return contracts.ExecResult{Stdout: []byte(out)}, nil
	default:
		return contracts.ExecResult{ExitCode: -1}, fmt.Errorf("%w: %q", fakes.ErrUnexpectedCommand, line)
	}
	return contracts.ExecResult{}, nil
}

// Set changes the flag behind the daemon's back (a manual pmset).
func (k *kernel) Set(flag bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.flag = flag
}

// SetStuck makes pmset -a disablesleep 0 fail silently (or work again).
func (k *kernel) SetStuck(stuck bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.stuck = stuck
}

func (k *kernel) Flag() bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.flag
}

func (k *kernel) Calls() []string {
	k.mu.Lock()
	defer k.mu.Unlock()
	return slices.Clone(k.calls)
}

func (k *kernel) Sleeps() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.sleepCnt
}

type harness struct {
	t       *testing.T
	s       *Server
	k       *kernel
	clock   *fakes.Clock
	thermal *fakes.Thermal
	power   *fakes.Power
	dir     string
	sock    string
	allow   string
	cancel  context.CancelFunc
	done    chan error
}

func shortDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "bckp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// newHarness builds a server whose allowlist contains the test's uid. The
// idle check is off so its timer does not disturb waitTimers counts;
// withIdleCheck turns it on.
func newHarness(t *testing.T) *harness {
	t.Helper()
	dir := shortDir(t)
	h := &harness{t: t, k: &kernel{flag: true}, clock: fakes.NewClock(start), dir: dir,
		sock: filepath.Join(dir, "d.sock"), allow: filepath.Join(dir, "allowed_uids")}
	h.thermal = fakes.NewThermal(h.clock, contracts.ThermalReading{State: contracts.ThermalNominal})
	h.power = fakes.NewPower(h.clock, contracts.PowerReading{OnAC: true, Percent: 80})
	h.writeAllow(fmt.Sprintf("%d\n", os.Getuid()), 0o644)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if testing.Verbose() {
		log = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	h.s = New(Deps{Exec: h.k, Clock: h.clock, Thermal: h.thermal, Power: h.power, Log: log},
		Options{Socket: h.sock, AllowedUIDs: h.allow, AllowlistOwner: os.Getuid(), IdleCheck: -1})
	return h
}

// withIdleCheck turns on the idle check, every d of fake time. Call it
// before start.
func (h *harness) withIdleCheck(d time.Duration) *harness {
	h.s.opt.IdleCheck = d
	return h
}

func (h *harness) writeAllow(content string, mode os.FileMode) {
	h.t.Helper()
	if err := os.WriteFile(h.allow, []byte(content), 0o600); err != nil {
		h.t.Fatal(err)
	}
	if err := os.Chmod(h.allow, mode); err != nil {
		h.t.Fatal(err)
	}
}

// start runs the server and waits until it accepts connections.
func (h *harness) start() {
	h.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel, h.done = cancel, make(chan error, 1)
	go func() { h.done <- h.s.Run(ctx) }()
	h.t.Cleanup(h.stop)
	h.eventually("socket listening", func() bool {
		c, err := net.Dial("unix", h.sock)
		if err == nil {
			c.Close()
		}
		return err == nil
	})
}

func (h *harness) stop() {
	if h.cancel == nil {
		return
	}
	h.cancel()
	h.cancel = nil
	select {
	case err := <-h.done:
		if err != nil {
			h.t.Errorf("Run: %v", err)
		}
	case <-time.After(waitBudget):
		h.t.Error("Run did not return")
	}
}

func (h *harness) eventually(what string, cond func() bool) {
	h.t.Helper()
	deadline := time.Now().Add(waitBudget)
	for !cond() {
		if time.Now().After(deadline) {
			h.t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// waitTimers waits until the lease supervisor is parked (expiry + poll).
func (h *harness) waitTimers(n int) {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), waitBudget)
	defer cancel()
	if err := h.clock.BlockUntilTimers(ctx, n); err != nil {
		h.t.Fatalf("waiting for %d timers: %v", n, err)
	}
}

// client is a raw protocol client.
type client struct {
	t *testing.T
	c net.Conn
	r *bufio.Reader
}

func (h *harness) dial() *client {
	h.t.Helper()
	c, err := net.Dial("unix", h.sock)
	if err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(func() { c.Close() })
	return &client{t: h.t, c: c, r: bufio.NewReader(c)}
}

func (c *client) send(line string) {
	c.t.Helper()
	if _, err := io.WriteString(c.c, line+"\n"); err != nil {
		c.t.Fatalf("write: %v", err)
	}
}

// recv reads one response line into a generic map.
func (c *client) recv() map[string]any {
	c.t.Helper()
	_ = c.c.SetReadDeadline(time.Now().Add(waitBudget))
	line, err := c.r.ReadString('\n')
	if err != nil {
		c.t.Fatalf("read: %v (partial %q)", err, line)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		c.t.Fatalf("bad response %q: %v", line, err)
	}
	return m
}

func (c *client) do(line string) map[string]any {
	c.t.Helper()
	c.send(line)
	return c.recv()
}

// closed reports whether the daemon closed the connection.
func (c *client) closed() bool {
	_ = c.c.SetReadDeadline(time.Now().Add(waitBudget))
	_, err := c.r.ReadByte()
	return err != nil
}

func (c *client) acquire(ttl int) string {
	c.t.Helper()
	m := c.do(fmt.Sprintf(`{"op":"acquire","ttl_s":%d,"reason":"test"}`, ttl))
	if m["ok"] != true {
		c.t.Fatalf("acquire: %v", m)
	}
	return m["lease"].(string)
}

func wantCode(t *testing.T, m map[string]any, code proto.ErrorCode) {
	t.Helper()
	if m["ok"] != false || m["code"] != string(code) {
		t.Fatalf("got %v, want code %s", m, code)
	}
}

func (h *harness) status() proto.DaemonStatusResponse {
	h.t.Helper()
	st, err := h.tryStatus()
	if err != nil {
		h.t.Fatal(err)
	}
	return st
}

// tryStatus is safe to call from any goroutine. The write error is
// ignored: a rejected connection is answered and closed before reading.
func (h *harness) tryStatus() (proto.DaemonStatusResponse, error) {
	var st proto.DaemonStatusResponse
	c, err := net.Dial("unix", h.sock)
	if err != nil {
		return st, err
	}
	defer c.Close()
	_, _ = io.WriteString(c, `{"op":"status"}`+"\n")
	_ = c.SetReadDeadline(time.Now().Add(waitBudget))
	line, err := bufio.NewReader(c).ReadBytes('\n')
	if err != nil {
		return st, err
	}
	return st, proto.Unmarshal(line, &st, true)
}

func hasSubseq(calls []string, want ...string) bool {
	i := 0
	for _, c := range calls {
		if i < len(want) && c == want[i] {
			i++
		}
	}
	return i == len(want)
}

func joinCalls(calls []string) string { return strings.Join(calls, "\n  ") }
