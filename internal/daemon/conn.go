package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/ilias-t/bckp/internal/contracts"
	"github.com/ilias-t/bckp/internal/eventlog"
	"github.com/ilias-t/bckp/internal/paths"
	"github.com/ilias-t/bckp/internal/proto"
)

// listen removes a stale socket and listens with mode 0666 (root:wheel
// when running as root). The uid allowlist, not the mode, gates access.
func listen(path string) (*net.UnixListener, error) {
	if err := paths.CheckSocketPath(path); err != nil {
		return nil, err
	}
	if fi, err := os.Lstat(path); err == nil {
		if fi.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("%s exists and is not a socket; refusing to remove it", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("remove stale socket: %w", err)
		}
	}
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, err
	}
	if os.Geteuid() == 0 {
		err = os.Chown(path, 0, 0)
	}
	if err == nil {
		err = os.Chmod(path, 0o666)
	}
	if err != nil {
		ln.Close()
		return nil, fmt.Errorf("socket permissions: %w", err)
	}
	return ln, nil
}

// accept registers c and serves it, or rejects it when the connection
// limit is reached.
func (s *Server) accept(c *net.UnixConn) {
	s.mu.Lock()
	full := len(s.conns) >= proto.MaxDaemonConns || s.closed
	if !full {
		s.conns[c] = struct{}{}
		s.wg.Add(1)
	}
	s.mu.Unlock()
	if full {
		s.logLimited(slog.LevelWarn, "conn.reject", "reason", "connection limit reached", "limit", proto.MaxDaemonConns)
		reply(c, proto.Fail(proto.CodeBusy, "too many connections"))
		c.Close()
		return
	}
	go s.serve(c)
}

func (s *Server) serve(c *net.UnixConn) {
	defer s.wg.Done()
	defer s.recoverReset()
	defer s.drop(c)
	uid, pid, err := peerCred(c)
	if err == nil {
		err = s.checkUID(uid)
	}
	if err != nil {
		s.logLimited(slog.LevelWarn, "auth.deny", "uid", uid, "pid", pid, "err", err)
		reply(c, proto.Fail(proto.CodeNotAllowed, "caller not allowed"))
		return
	}
	lr := proto.NewLineReader(c, proto.MaxDaemonRequestBytes)
	for {
		deadline := time.Now().Add(idleTimeout)
		if s.holdsLease(c) {
			deadline = time.Time{} // lease expiry bounds this connection
		}
		_ = c.SetReadDeadline(deadline)
		var req proto.DaemonRequest
		err := lr.Decode(&req, true)
		var de *proto.DecodeError
		switch {
		case errors.Is(err, proto.ErrLineTooLong):
			reply(c, proto.Fail(proto.CodeTooLarge, "request exceeds %d bytes", proto.MaxDaemonRequestBytes))
			return
		case errors.As(err, &de):
			reply(c, proto.Fail(proto.CodeBadRequest, "%v", err))
			return
		case err != nil: // EOF, timeout, closed
			return
		}
		if err := req.Validate(); err != nil {
			var re *proto.RemoteError
			errors.As(err, &re)
			if reply(c, proto.Fail(re.Code, "%s", re.Message)) != nil {
				return
			}
			continue
		}
		resp, after := s.handle(c, int(uid), pid, &req)
		werr := reply(c, resp)
		if after != nil {
			after()
		}
		if werr != nil {
			return
		}
	}
}

// drop closes c and, if it holds the lease, ends the lease immediately.
func (s *Server) drop(c net.Conn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c.Close()
	delete(s.conns, c)
	if s.lease != nil && s.lease.conn == c {
		_ = s.endLeaseLocked("lease.conn_closed", "lease connection closed (engine exited or crashed)", false)
	}
}

func (s *Server) holdsLease(c net.Conn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lease != nil && s.lease.conn == c
}

func reply(c net.Conn, v any) error {
	_ = c.SetWriteDeadline(time.Now().Add(writeTimeout))
	return proto.WriteLine(c, v)
}

// peerCred returns the peer's uid (LOCAL_PEERCRED) and pid (LOCAL_PEERPID).
func peerCred(c *net.UnixConn) (uid uint32, pid int, err error) {
	raw, err := c.SyscallConn()
	if err != nil {
		return 0, 0, err
	}
	cerr := raw.Control(func(fd uintptr) {
		var x *unix.Xucred
		if x, err = unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED); err != nil {
			return
		}
		uid = x.Uid
		pid, _ = unix.GetsockoptInt(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERPID)
	})
	if cerr != nil {
		return 0, 0, cerr
	}
	return uid, pid, err
}

// checkUID re-reads the allowlist on every connection and fails closed.
func (s *Server) checkUID(uid uint32) error {
	allowed, err := readAllowlist(s.opt.AllowedUIDs, s.opt.AllowlistOwner)
	if err != nil {
		return err
	}
	if !allowed[uid] {
		return fmt.Errorf("uid %d not in allowlist", uid)
	}
	return nil
}

const maxAllowlistBytes = 4096

// readAllowlist reads the uid allowlist. It must be a regular file (not a
// symlink) owned by owner and not writable by group or others.
func readAllowlist(path string, owner int) (map[uint32]bool, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("allowlist: %w", err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("allowlist: %w", err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	switch {
	case !fi.Mode().IsRegular():
		return nil, errors.New("allowlist: not a regular file")
	case !ok || int(st.Uid) != owner:
		return nil, fmt.Errorf("allowlist: not owned by uid %d", owner)
	case fi.Mode().Perm()&0o022 != 0:
		return nil, errors.New("allowlist: writable by group or others")
	}
	b, err := io.ReadAll(io.LimitReader(f, maxAllowlistBytes+1))
	if err != nil {
		return nil, fmt.Errorf("allowlist: %w", err)
	}
	if len(b) > maxAllowlistBytes {
		return nil, errors.New("allowlist: too large")
	}
	return parseAllowlist(string(b))
}

// parseAllowlist accepts one decimal uid per line, each line terminated
// by '\n' (the last terminator is optional). Nothing else is allowed: no
// blanks, spaces, signs or comments. An empty list is an error.
func parseAllowlist(s string) (map[uint32]bool, error) {
	s = strings.TrimSuffix(s, "\n")
	if s == "" {
		return nil, errors.New("allowlist: empty")
	}
	out := map[uint32]bool{}
	for i, line := range strings.Split(s, "\n") {
		if line == "" || strings.Trim(line, "0123456789") != "" {
			return nil, fmt.Errorf("allowlist: line %d: not a decimal uid", i+1)
		}
		uid, err := strconv.ParseUint(line, 10, 32)
		if err != nil {
			return nil, fmt.Errorf("allowlist: line %d: %w", i+1, err)
		}
		out[uint32(uid)] = true
	}
	return out, nil
}

// backstopState carries consecutive-reading state across polls.
type backstopState struct {
	critical, battHot int
	thermErr, powrErr bool // log read errors on transitions only
}

// backstopCheck reads both sources once and returns a trip reason, or "".
// A thermal read error (or unknown state) is not critical and does not
// reset the count; neither does a missing battery °C.
func (s *Server) backstopCheck(bs *backstopState) string {
	ctx, cancel := context.WithTimeout(context.Background(), pollTimeout)
	defer cancel()
	reason := ""
	th, err := s.d.Thermal.Read(ctx)
	if err == nil && th.State == contracts.ThermalUnknown {
		err = errors.New("thermal state unknown") // a failed reading, not "cool"
	}
	s.noteReadErr(&bs.thermErr, "thermal", err)
	if err == nil {
		if th.State == contracts.ThermalCritical {
			bs.critical++
		} else {
			bs.critical = 0
		}
		if b := th.BatteryC; b != nil {
			if *b >= proto.BackstopBatteryTempC {
				bs.battHot++
			} else {
				bs.battHot = 0
			}
		}
		switch {
		case bs.critical >= proto.BackstopCriticalPolls:
			reason = fmt.Sprintf("thermal critical x%d", bs.critical)
		case bs.battHot >= proto.BackstopBatteryTempPolls:
			reason = fmt.Sprintf("battery %.0f°C x%d", *th.BatteryC, bs.battHot)
		}
	}
	pw, err := s.d.Power.Read(ctx)
	s.noteReadErr(&bs.powrErr, "power", err)
	if err == nil && reason == "" && !pw.OnAC && pw.Percent < proto.BackstopBatteryPercent {
		reason = fmt.Sprintf("battery %d%%", pw.Percent)
	}
	return reason
}

func (s *Server) noteReadErr(failing *bool, source string, err error) {
	switch {
	case err != nil && !*failing:
		eventlog.Log(s.log, slog.LevelWarn, "backstop.read_error", "source", source, "err", err)
	case err == nil && *failing:
		eventlog.Log(s.log, slog.LevelInfo, "backstop.read_recovered", "source", source)
	}
	*failing = err != nil
}

// Rate limit for events any local process can trigger (auth.deny,
// conn.reject): the first logBurst per logWindow are logged, the rest are
// counted and reported in one log.suppressed line.
const (
	logBurst  = 10
	logWindow = time.Minute
)

type limitWindow struct {
	start              time.Time
	logged, suppressed int
}

// logLimited logs one rate-limited event.
func (s *Server) logLimited(level slog.Level, name string, args ...any) {
	now := s.d.Clock.Now()
	s.limitMu.Lock()
	w := s.limited[name]
	var ended *limitWindow
	if w != nil && now.Sub(w.start) >= logWindow {
		ended, w = w, nil
	}
	if w == nil {
		w = &limitWindow{start: now}
		s.limited[name] = w
	}
	emit := w.logged < logBurst
	if emit {
		w.logged++
	} else {
		w.suppressed++
	}
	s.limitMu.Unlock()
	if ended != nil {
		s.logSuppressed(name, ended, now)
	}
	if emit {
		eventlog.Log(s.log, level, name, args...)
	}
}

// flushSuppressed reports and closes windows that have ended (all of them
// with force), so a burst is summarised even if nothing follows it.
func (s *Server) flushSuppressed(force bool) {
	now := s.d.Clock.Now()
	s.limitMu.Lock()
	ended := map[string]*limitWindow{}
	for name, w := range s.limited {
		if force || now.Sub(w.start) >= logWindow {
			ended[name] = w
			delete(s.limited, name)
		}
	}
	s.limitMu.Unlock()
	for name, w := range ended {
		s.logSuppressed(name, w, now)
	}
}

func (s *Server) logSuppressed(name string, w *limitWindow, now time.Time) {
	if w.suppressed == 0 {
		return
	}
	eventlog.Log(s.log, slog.LevelWarn, "log.suppressed", "suppressed_event", name, "count", w.suppressed,
		"logged", w.logged, "over", now.Sub(w.start).Round(time.Second).String())
}
