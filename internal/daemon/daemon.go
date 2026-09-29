// Package daemon is backpackd: the root LaunchDaemon that owns the global
// `pmset -a disablesleep` flag. It is deliberately small and conservative.
//
// Invariants:
//   - The only commands ever run are the argv arrays below; nothing a
//     client sends reaches argv.
//   - disablesleep is reset to 0 on start (before listening), on shutdown,
//     when the lease connection closes, when the lease expires, when a
//     backstop trips, and before every sleepnow. Every change is verified
//     against the kernel's SleepDisabled value (ioreg) and retried once.
//   - At most one lease exists; it is bound to the connection that
//     acquired it.
//   - Without a lease, SleepDisabled is read every Options.IdleCheck (60s)
//     and reset when it reads Yes, so a failed reset is retried until it
//     verifies. This also undoes a manual `sudo pmset -a disablesleep 1`
//     while the helper is installed.
//
// The wire protocol is documented in internal/proto/daemon.go.
package daemon

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/ilias-t/bckp/internal/contracts"
	"github.com/ilias-t/bckp/internal/eventlog"
	"github.com/ilias-t/bckp/internal/paths"
	"github.com/ilias-t/bckp/internal/proto"
	"github.com/ilias-t/bckp/internal/providers/power"
	"github.com/ilias-t/bckp/internal/version"
)

// The complete set of commands backpackd runs. Hard-coded; never extended
// with client data.
var (
	argvSleepOff = [...]string{"-a", "disablesleep", "0"}
	argvSleepOn  = [...]string{"-a", "disablesleep", "1"}
	argvSleepNow = [...]string{"sleepnow"}
	// ioreg, read-only; the same argv as power.ClamshellArgs (a test keeps
	// them equal), parsed with power.ParseClamshell.
	argvReadFlag = [...]string{"-r", "-k", "AppleClamshellState", "-d", "4"}
)

const (
	execTimeout  = 10 * time.Second // per pmset/ioreg run
	pollTimeout  = 5 * time.Second  // per backstop source read
	idleTimeout  = 60 * time.Second // read timeout for connections without a lease
	writeTimeout = 5 * time.Second
	// idleCheck is how often SleepDisabled is checked without a lease.
	idleCheck = 60 * time.Second
)

// Deps are backpackd's injected dependencies. All are required.
type Deps struct {
	Exec    contracts.Exec
	Clock   contracts.Clock
	Thermal contracts.ThermalSource
	Power   contracts.PowerSource
	// Log defaults to the event log in LogDir, else a text logger on
	// stderr.
	Log *slog.Logger
	// LogDir, when set and Log is nil, makes Main log JSONL events to
	// LogDir/backpackd.jsonl (eventlog.SystemDir in production: root:wheel
	// 0755, files 0644 so the user can read them). If it cannot be opened,
	// Main logs to stderr and carries on.
	LogDir string
}

// Options are locations. Zero values mean the production defaults.
type Options struct {
	// Socket defaults to paths.DaemonSocket.
	Socket string
	// AllowedUIDs defaults to paths.AllowedUIDs.
	AllowedUIDs string
	// AllowlistOwner is the uid that must own the allowlist: 0 (root) in
	// production. Tests set their own uid.
	AllowlistOwner int
	// IdleCheck is how often SleepDisabled is checked (and reset) while no
	// lease is held. Zero means 60s; negative disables the check (tests).
	IdleCheck time.Duration
}

// Server is the daemon. Create it with New and run it with Run.
type Server struct {
	d   Deps
	log *slog.Logger
	opt Options

	mu            sync.Mutex
	closed        bool
	conns         map[net.Conn]struct{}
	lease         *lease
	sleepDisabled bool   // last value read from ioreg
	backstop      string // contracts.Backstop*
	idleReadErr   bool   // the idle check's ioreg read is failing
	wg            sync.WaitGroup

	limitMu sync.Mutex
	limited map[string]*limitWindow // rate-limited events by name
}

type lease struct {
	id               string
	conn             net.Conn
	ttl              time.Duration
	reason           string
	uid, pid         int
	acquired, expiry time.Time
	done             chan struct{} // closed when the lease ends
}

// Main runs backpackd until SIGTERM/SIGINT and returns the exit code. It
// refuses to serve (after resetting disablesleep to 0) when the backstop
// sources are not wired.
func Main(d Deps) int {
	if d.Log == nil && d.LogDir != "" {
		l, c, err := eventlog.Open(d.LogDir, eventlog.DaemonName, eventlog.Options{
			Src: eventlog.SrcDaemon, DirMode: 0o755, FileMode: 0o644})
		if err != nil {
			d.logger().Error("event log unavailable; logging to stderr", "dir", d.LogDir, "err", err)
		} else {
			d.Log = l
			defer c.Close()
		}
	}
	log := d.logger()
	if d.Exec == nil || d.Clock == nil {
		log.Error("refusing to start: Exec and Clock are required")
		return 2
	}
	s := New(d, Options{})
	if d.Thermal == nil || d.Power == nil {
		s.mu.Lock()
		_ = s.setFlag(false)
		s.mu.Unlock()
		log.Error("refusing to start: thermal and power sources are required for the backstop")
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	if err := s.Run(ctx); err != nil {
		log.Error("exiting", "err", err)
		return 1
	}
	return 0
}

func (d Deps) logger() *slog.Logger {
	if d.Log != nil {
		return d.Log
	}
	return slog.New(slog.NewTextHandler(os.Stderr, nil))
}

// New returns a server; it does nothing until Run.
func New(d Deps, opt Options) *Server {
	if opt.Socket == "" {
		opt.Socket = paths.DaemonSocket
	}
	if opt.AllowedUIDs == "" {
		opt.AllowedUIDs = paths.AllowedUIDs
	}
	if opt.IdleCheck == 0 {
		opt.IdleCheck = idleCheck
	}
	return &Server{d: d, log: d.logger(), opt: opt, conns: map[net.Conn]struct{}{}, backstop: contracts.BackstopIdle,
		limited: map[string]*limitWindow{}}
}

// Run resets disablesleep to 0, serves until ctx ends, then resets again.
// It returns an error on any fatal condition (after the reset).
func (s *Server) Run(ctx context.Context) error {
	defer s.recoverReset()
	s.mu.Lock()
	err := s.setFlag(false) // first action, before listening
	s.mu.Unlock()
	if err != nil {
		return fmt.Errorf("reset on start: %w", err)
	}
	ln, err := listen(s.opt.Socket)
	if err != nil {
		eventlog.Log(s.log, slog.LevelError, "daemon.error", "err", err, "step", "listen")
		s.shutdown("listen failed")
		return err
	}
	eventlog.Log(s.log, slog.LevelInfo, "daemon.start", "version", version.String(), "pid", os.Getpid(),
		"socket", s.opt.Socket, "sleep_disabled", s.sleepDisabled)
	stop := context.AfterFunc(ctx, func() { ln.Close() })
	defer stop()
	if s.opt.IdleCheck > 0 {
		stopIdle, idleDone := make(chan struct{}), make(chan struct{})
		go func() {
			defer close(idleDone)
			s.idleLoop(stopIdle)
		}()
		defer func() {
			close(stopIdle)
			<-idleDone
		}()
	}
	for {
		c, err := ln.AcceptUnix()
		if err != nil {
			if ctx.Err() != nil {
				s.shutdown("signal")
				return nil
			}
			ln.Close()
			s.shutdown("accept failed")
			return fmt.Errorf("accept: %w", err)
		}
		s.accept(c)
	}
}

// shutdown closes every connection, ends any lease, resets the flag, and
// waits for connection handlers to finish.
func (s *Server) shutdown(why string) {
	s.mu.Lock()
	s.closed = true
	for c := range s.conns {
		c.Close()
	}
	if s.lease != nil {
		s.endLeaseLocked("lease.end", "shutdown: "+why, true)
	} else {
		_ = s.setFlag(false)
	}
	s.mu.Unlock()
	s.wg.Wait()
	s.flushSuppressed(true)
	eventlog.Log(s.log, slog.LevelInfo, "daemon.stop", "reason", why)
}

// recoverReset resets the flag on a panic, then re-panics so launchd
// restarts the daemon (which resets again on start).
func (s *Server) recoverReset() {
	if r := recover(); r != nil {
		eventlog.Log(s.log, slog.LevelError, "daemon.panic", "panic", fmt.Sprint(r), "note", "resetting disablesleep")
		_ = s.trySetFlag(false)
		panic(r)
	}
}

// handle executes one validated request under s.mu. The returned func, if
// any, runs after the response was written (sleep's pmset sleepnow).
func (s *Server) handle(c net.Conn, uid, pid int, req *proto.DaemonRequest) (any, func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return proto.Fail(proto.CodeInternal, "shutting down"), nil
	}
	if req.Op == proto.OpStatus {
		return s.statusLocked(), nil
	}
	if req.Op == proto.OpAcquire {
		return s.acquireLocked(c, uid, pid, req), nil
	}
	l := s.lease
	switch {
	case l == nil && req.Op == proto.OpSleep && req.Lease == "":
		return s.sleepLocked(req.Reason)
	case l == nil:
		return proto.Fail(proto.CodeNoLease, "no active lease"), nil
	case req.Lease == "": // sleep without id while a lease is active
		return proto.Fail(proto.CodeBusy, "another lease is active"), nil
	case req.Lease != l.id || l.conn != c:
		return proto.Fail(proto.CodeLeaseMismatch, "lease id or connection does not match"), nil
	}
	switch req.Op {
	case proto.OpRenew:
		l.expiry = s.d.Clock.Now().Add(l.ttl)
		eventlog.Log(s.log, slog.LevelDebug, "lease.renew", "lease", l.id, "expires", l.expiry)
		return proto.DaemonLeaseResponse{Result: proto.OKResult(), Lease: l.id, Expires: l.expiry}, nil
	case proto.OpRelease:
		if err := s.endLeaseLocked("lease.release", "released by client", false); err != nil {
			return proto.Fail(proto.CodeInternal, "disablesleep 0 did not verify: %v", err), nil
		}
		return proto.OKResult(), nil
	default: // OpSleep
		return s.sleepLocked(req.Reason)
	}
}

func (s *Server) acquireLocked(c net.Conn, uid, pid int, req *proto.DaemonRequest) any {
	if s.lease != nil {
		return proto.Fail(proto.CodeBusy, "another lease is active")
	}
	if err := s.setFlag(true); err != nil {
		_ = s.setFlag(false)
		eventlog.Log(s.log, slog.LevelError, "lease.acquire", "uid", uid, "pid", pid, "result", "failed", "err", err)
		return proto.Fail(proto.CodeInternal, "disablesleep 1 failed: %v", err)
	}
	var b [8]byte
	_, _ = rand.Read(b[:]) // never fails on darwin
	now := s.d.Clock.Now()
	ttl := time.Duration(req.TTLS) * time.Second // clamped by Validate
	l := &lease{id: hex.EncodeToString(b[:]), conn: c, ttl: ttl, reason: req.Reason, uid: uid, pid: pid,
		acquired: now, expiry: now.Add(ttl), done: make(chan struct{})}
	s.lease, s.backstop = l, contracts.BackstopArmed
	eventlog.Log(s.log, slog.LevelInfo, "lease.acquire", "lease", l.id, "uid", uid, "pid", pid, "ttl", ttl.String(), "reason", l.reason)
	go s.supervise(l)
	return proto.DaemonLeaseResponse{Result: proto.OKResult(), Lease: l.id, Expires: l.expiry}
}

// sleepLocked resets and verifies the flag, ends any lease, and returns
// the sleepnow step to run after the ok response is written.
func (s *Server) sleepLocked(reason string) (any, func()) {
	if err := s.setFlag(false); err != nil {
		eventlog.Log(s.log, slog.LevelError, "sleep.op", "reason", reason, "result", "refused", "err", err)
		return proto.Fail(proto.CodeInternal, "disablesleep 0 did not verify; not sleeping: %v", err), nil
	}
	if s.lease != nil {
		s.clearLeaseLocked("lease.end", "sleep requested", false)
	}
	eventlog.Log(s.log, slog.LevelInfo, "sleep.op", "reason", reason, "result", "ok")
	return proto.OKResult(), func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.sleepNowLocked()
	}
}

func (s *Server) statusLocked() proto.DaemonStatusResponse {
	st := contracts.GuardStatus{SleepDisabled: s.sleepDisabled, Backstop: s.backstop}
	if l := s.lease; l != nil {
		st.Lease = &contracts.LeaseInfo{ID: l.id, Reason: l.reason, UID: l.uid, PID: l.pid,
			TTLS: int(l.ttl / time.Second), AcquiredAt: l.acquired, Expires: l.expiry}
	}
	return proto.DaemonStatusResponse{Result: proto.OKResult(), GuardStatus: st}
}

// endLeaseLocked resets the flag and ends the lease. With closeConn, the
// lease connection is closed so the client observes the loss.
// event is the lease's end event: lease.release, lease.expire,
// lease.conn_closed or lease.end (shutdown, sleep, backstop).
func (s *Server) endLeaseLocked(event, why string, closeConn bool) error {
	err := s.setFlag(false)
	s.clearLeaseLocked(event, why, closeConn)
	return err
}

func (s *Server) clearLeaseLocked(event, why string, closeConn bool) {
	l := s.lease
	s.lease, s.backstop = nil, contracts.BackstopIdle
	close(l.done)
	if closeConn {
		l.conn.Close()
	}
	level := slog.LevelInfo
	if event != "lease.release" {
		level = slog.LevelWarn
	}
	eventlog.Log(s.log, level, event, "lease", l.id, "why", why, "held", s.d.Clock.Now().Sub(l.acquired).Round(time.Second).String(),
		"sleep_disabled", s.sleepDisabled)
}

// supervise enforces the lease's TTL and runs the backstop poll until the
// lease ends.
func (s *Server) supervise(l *lease) {
	defer s.recoverReset()
	var bs backstopState
	expiry := s.d.Clock.After(s.untilExpiry(l))
	poll := s.d.Clock.After(proto.BackstopPollInterval)
	for {
		select {
		case <-l.done:
			return
		case <-expiry:
			s.mu.Lock()
			if s.lease == l && !s.d.Clock.Now().Before(l.expiry) {
				_ = s.endLeaseLocked("lease.expire", "not renewed within the ttl", true)
			}
			s.mu.Unlock()
			expiry = s.d.Clock.After(s.untilExpiry(l))
		case <-poll:
			if reason := s.backstopCheck(&bs); reason != "" {
				s.trip(l, reason)
			}
			poll = s.d.Clock.After(proto.BackstopPollInterval)
		}
	}
}

func (s *Server) untilExpiry(l *lease) time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return l.expiry.Sub(s.d.Clock.Now())
}

// trip ends the lease for a backstop reason and puts the Mac to sleep.
func (s *Server) trip(l *lease, reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lease != l {
		return
	}
	eventlog.Log(s.log, slog.LevelError, "backstop.trip", "reason", reason, "lease", l.id)
	_ = s.endLeaseLocked("lease.end", "backstop: "+reason, true)
	s.backstop = contracts.BackstopTrippedPrefix + reason
	s.sleepNowLocked() // even if the reset did not verify: this is an emergency
}

func (s *Server) sleepNowLocked() {
	if _, err := s.run(paths.BinPmset, argvSleepNow[:]); err != nil {
		eventlog.Log(s.log, slog.LevelError, "sleep.now", "result", "failed", "err", err)
		return
	}
	eventlog.Log(s.log, slog.LevelInfo, "sleep.now", "result", "ok")
}

// setFlag sets disablesleep and verifies it via ioreg, retrying once.
// Callers hold s.mu.
func (s *Server) setFlag(on bool) error {
	err := s.trySetFlag(on)
	if err != nil {
		eventlog.Log(s.log, slog.LevelWarn, "flag.set", "value", on, "verified", false, "attempt", 1, "err", err)
		if err = s.trySetFlag(on); err != nil {
			eventlog.Log(s.log, slog.LevelError, "flag.set", "value", on, "verified", false, "attempt", 2, "err", err)
			return err
		}
	}
	eventlog.Log(s.log, slog.LevelInfo, "flag.set", "value", on, "verified", true)
	return nil
}

func (s *Server) trySetFlag(on bool) error {
	argv := argvSleepOff
	if on {
		argv = argvSleepOn
	}
	_, perr := s.run(paths.BinPmset, argv[:])
	got, err := s.readFlag()
	if err != nil {
		return errors.Join(perr, err)
	}
	if got != on {
		return errors.Join(perr, fmt.Errorf("kernel reports SleepDisabled=%s", yesNo(got)))
	}
	return nil
}

// readFlag reads the kernel's SleepDisabled through ioreg and records it.
// Output without the key is an error, so it never verifies. Callers hold
// s.mu.
func (s *Server) readFlag() (bool, error) {
	out, err := s.run(paths.BinIoreg, argvReadFlag[:])
	if err != nil {
		return false, err
	}
	c, err := power.ParseClamshell(string(out))
	if err != nil {
		return false, err
	}
	s.sleepDisabled = c.SleepDisabled
	return c.SleepDisabled, nil
}

func yesNo(b bool) string {
	if b {
		return "Yes"
	}
	return "No"
}

// idleLoop runs the idle check every Options.IdleCheck until stop closes.
func (s *Server) idleLoop(stop <-chan struct{}) {
	defer s.recoverReset()
	for {
		select {
		case <-stop:
			return
		case <-s.d.Clock.After(s.opt.IdleCheck):
		}
		s.reconcileIdle()
		s.flushSuppressed(false)
	}
}

// reconcileIdle resets SleepDisabled when it reads Yes with no lease: after
// a reset that did not verify (a failed release, expiry or backstop), or
// after someone ran `pmset -a disablesleep 1` by hand. Without a lease
// nothing may keep it on, so a manual setting is undone within a minute
// while the helper is installed. It does nothing while a lease is held.
func (s *Server) reconcileIdle() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.lease != nil {
		return
	}
	on, err := s.readFlag()
	switch {
	case err != nil && !s.idleReadErr:
		eventlog.Log(s.log, slog.LevelWarn, "flag.reconcile", "result", "read_failed", "err", err)
	case err == nil && s.idleReadErr:
		eventlog.Log(s.log, slog.LevelInfo, "flag.reconcile", "result", "read_ok", "sleep_disabled", on)
	}
	s.idleReadErr = err != nil
	if err != nil || !on {
		return
	}
	if err := s.setFlag(false); err != nil {
		eventlog.Log(s.log, slog.LevelError, "flag.reconcile", "result", "failed", "err", err,
			"note", "SleepDisabled=Yes without a lease; retrying in "+s.opt.IdleCheck.String())
		return
	}
	eventlog.Log(s.log, slog.LevelWarn, "flag.reconcile", "result", "reset",
		"note", "SleepDisabled was Yes without a lease")
}

func (s *Server) run(name string, argv []string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), execTimeout)
	defer cancel()
	res, err := s.d.Exec.Run(ctx, name, argv...)
	return res.Stdout, err
}
