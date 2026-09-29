// Package client implements contracts.SleepGuard and contracts.Lease over
// backpackd's socket protocol (internal/proto/daemon.go).
package client

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/ilias-t/bckp/internal/contracts"
	"github.com/ilias-t/bckp/internal/proto"
)

// New returns a SleepGuard that talks to backpackd at socketPath
// (normally paths.DaemonSocket). Leases never renew themselves: the
// holder's loop calls Renew, so a hung holder loses the lease within its
// TTL.
func New(socketPath string) contracts.SleepGuard { return &guard{path: socketPath} }

type guard struct {
	path  string
	mu    sync.Mutex
	lease *lease // most recent lease from Acquire
}

// response is any daemon response type; they all embed proto.Result.
type response interface{ Err() error }

func (g *guard) dial(ctx context.Context) (net.Conn, error) {
	var d net.Dialer
	c, err := d.DialContext(ctx, "unix", g.path)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", contracts.ErrDaemonUnavailable, err)
	}
	return c, nil
}

// oneShot sends req on a fresh connection and decodes one response.
func (g *guard) oneShot(ctx context.Context, req proto.DaemonRequest, out response) error {
	c, err := g.dial(ctx)
	if err != nil {
		return err
	}
	defer c.Close()
	stop := context.AfterFunc(ctx, func() { _ = c.SetDeadline(time.Unix(1, 0)) })
	defer stop()
	werr := proto.WriteLine(c, req)
	// Read even if the write failed: the daemon may have answered (e.g.
	// not_allowed) and closed before reading.
	line, err := proto.NewLineReader(c, proto.MaxDaemonResponseBytes).ReadLine()
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("%w: %w", contracts.ErrDaemonUnavailable, errors.Join(werr, err))
	}
	if err := proto.Unmarshal(line, out, false); err != nil {
		return err
	}
	return out.Err()
}

func (g *guard) Status(ctx context.Context) (contracts.GuardStatus, error) {
	var resp proto.DaemonStatusResponse
	if err := g.oneShot(ctx, proto.DaemonRequest{Op: proto.OpStatus}, &resp); err != nil {
		return contracts.GuardStatus{}, err
	}
	return resp.GuardStatus, nil
}

func (g *guard) Acquire(ctx context.Context, ttl time.Duration, reason string) (contracts.Lease, error) {
	c, err := g.dial(ctx)
	if err != nil {
		return nil, err
	}
	l := &lease{c: c, lines: make(chan []byte, 1), dead: make(chan struct{}),
		ended: make(chan struct{}), lost: make(chan error, 1)}
	go l.read(proto.NewLineReader(c, proto.MaxDaemonResponseBytes))
	req := proto.DaemonRequest{Op: proto.OpAcquire, TTLS: int(ttl / time.Second), Reason: reason}
	var resp proto.DaemonLeaseResponse
	if err := l.request(ctx, req, &resp); err != nil {
		l.finish(nil)
		if proto.CodeOf(err) == "" && ctx.Err() == nil {
			err = fmt.Errorf("%w: acquire: %v", contracts.ErrDaemonUnavailable, err)
		}
		return nil, err
	}
	l.id, l.expires = resp.Lease, resp.Expires
	g.mu.Lock()
	g.lease = l
	g.mu.Unlock()
	return l, nil
}

func (g *guard) SleepNow(ctx context.Context, reason string) error {
	req := proto.DaemonRequest{Op: proto.OpSleep, Reason: reason}
	g.mu.Lock()
	l := g.lease
	g.mu.Unlock()
	if l == nil || l.isEnded() {
		return g.oneShot(ctx, req, &proto.Result{})
	}
	req.Lease = l.id
	if err := l.request(ctx, req, &proto.Result{}); err != nil {
		return err
	}
	l.finish(nil)
	return nil
}

// lease is one lease connection. A reader goroutine delivers response
// lines and detects the daemon closing the connection; requests are
// serialized by reqMu.
type lease struct {
	c     net.Conn
	id    string
	lines chan []byte   // response lines from the reader
	dead  chan struct{} // closed when the reader stops
	reqMu sync.Mutex

	mu      sync.Mutex
	expires time.Time
	done    bool
	ended   chan struct{} // closed when the lease ends (stops the reader)
	lost    chan error
}

func (l *lease) ID() string         { return l.id }
func (l *lease) Lost() <-chan error { return l.lost }

func (l *lease) Expires() time.Time {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.expires
}

func (l *lease) isEnded() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.done
}

func (l *lease) Renew(ctx context.Context) error {
	var resp proto.DaemonLeaseResponse
	if err := l.request(ctx, proto.DaemonRequest{Op: proto.OpRenew, Lease: l.id}, &resp); err != nil {
		return err
	}
	l.mu.Lock()
	l.expires = resp.Expires
	l.mu.Unlock()
	return nil
}

func (l *lease) Release(ctx context.Context) error {
	if l.isEnded() {
		return nil
	}
	err := l.request(ctx, proto.DaemonRequest{Op: proto.OpRelease, Lease: l.id}, &proto.Result{})
	l.finish(nil) // closing the connection also makes the daemon release
	if errors.Is(err, contracts.ErrLeaseLost) {
		return nil // already ended
	}
	return err
}

// request sends req on the lease connection and decodes the response.
// Any transport failure or cancellation drops the connection (the daemon
// then releases), ending the lease as lost.
func (l *lease) request(ctx context.Context, req proto.DaemonRequest, out response) error {
	l.reqMu.Lock()
	defer l.reqMu.Unlock()
	if l.isEnded() {
		return fmt.Errorf("%s: %w", req.Op, contracts.ErrLeaseLost)
	}
	deadline, _ := ctx.Deadline()
	_ = l.c.SetWriteDeadline(deadline)
	// After a failed write, still collect an answer the daemon may already
	// have sent (e.g. not_allowed), but don't wait long for it.
	var giveUp <-chan time.Time
	werr := proto.WriteLine(l.c, req)
	if werr != nil {
		giveUp = time.After(time.Second)
	}
	var line []byte
	select {
	case line = <-l.lines:
	case <-l.dead:
		select {
		case line = <-l.lines: // answered, then closed
		default:
			return l.fail(fmt.Errorf("%s: connection to backpackd closed: %w", req.Op, errors.Join(werr, net.ErrClosed)))
		}
	case <-giveUp:
		return l.fail(fmt.Errorf("%s: %w", req.Op, werr))
	case <-ctx.Done():
		return l.fail(fmt.Errorf("%s: %w", req.Op, ctx.Err()))
	}
	if err := proto.Unmarshal(line, out, false); err != nil {
		return l.fail(err)
	}
	err := out.Err()
	if errors.Is(err, contracts.ErrLeaseLost) {
		l.fail(err)
	}
	return err
}

func (l *lease) read(lr *proto.LineReader) {
	defer close(l.dead)
	for {
		line, err := lr.ReadLine()
		if err != nil {
			l.finish(lostErr(fmt.Errorf("connection to backpackd closed: %w", err)))
			return
		}
		select {
		case l.lines <- line:
		case <-l.ended:
			return
		}
	}
}

// fail ends the lease as lost and returns the loss error.
func (l *lease) fail(err error) error {
	err = lostErr(err)
	l.finish(err)
	return err
}

// finish ends the lease once: Lost receives err (if non-nil) and closes,
// and the connection is closed.
func (l *lease) finish(err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.done {
		return
	}
	l.done = true
	if err != nil {
		l.lost <- err
	}
	close(l.lost)
	close(l.ended)
	l.c.Close()
}

func lostErr(err error) error {
	if errors.Is(err, contracts.ErrLeaseLost) {
		return err
	}
	return fmt.Errorf("%w: %w", contracts.ErrLeaseLost, err)
}
