package fakes

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/ilias-t/bckp/internal/contracts"
)

// AcquireCall records one SleepGuard.Acquire.
type AcquireCall struct {
	TTL    time.Duration
	Reason string
}

// SleepGuard is a fake contracts.SleepGuard that models the daemon's lease
// rules: one active lease at a time (ErrLeaseBusy otherwise), SleepNow
// ends the active lease, and Status reflects the model unless overridden.
// It never touches pmset.
type SleepGuard struct {
	mu        sync.Mutex
	clock     contracts.Clock
	acquires  []AcquireCall
	sleeps    []string
	leases    []*Lease
	active    *Lease
	sleptOK   bool
	acquireEr []error
	sleepErr  error
	statusErr error
	status    *contracts.GuardStatus
}

var _ contracts.SleepGuard = (*SleepGuard)(nil)

// NewSleepGuard returns a fake guard. clock drives lease expiry values;
// nil uses the wall clock.
func NewSleepGuard(clock contracts.Clock) *SleepGuard {
	if clock == nil {
		clock = wallClock{}
	}
	return &SleepGuard{clock: clock}
}

type wallClock struct{}

func (wallClock) Now() time.Time                         { return time.Now() }
func (wallClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// FailAcquire makes the next Acquire calls fail, one error each.
func (g *SleepGuard) FailAcquire(errs ...error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.acquireEr = append(g.acquireEr, errs...)
}

// SetSleepErr makes SleepNow fail with err (nil clears).
func (g *SleepGuard) SetSleepErr(err error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.sleepErr = err
}

// SetStatus overrides Status's answer; nil restores the modeled status.
func (g *SleepGuard) SetStatus(s *contracts.GuardStatus, err error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.status, g.statusErr = s, err
}

func (g *SleepGuard) Acquire(ctx context.Context, ttl time.Duration, reason string) (contracts.Lease, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.acquires = append(g.acquires, AcquireCall{TTL: ttl, Reason: reason})
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(g.acquireEr) > 0 {
		err := g.acquireEr[0]
		g.acquireEr = g.acquireEr[1:]
		return nil, err
	}
	if g.active != nil && !g.active.Ended() {
		return nil, fmt.Errorf("fakes: acquire: %w", contracts.ErrLeaseBusy)
	}
	l := &Lease{
		id:      fmt.Sprintf("lease-%d", len(g.leases)+1),
		ttl:     ttl,
		clock:   g.clock,
		expires: g.clock.Now().Add(ttl),
		lost:    make(chan error, 1),
	}
	g.leases = append(g.leases, l)
	g.active = l
	g.sleptOK = false
	return l, nil
}

func (g *SleepGuard) SleepNow(ctx context.Context, reason string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.sleeps = append(g.sleeps, reason)
	if err := ctx.Err(); err != nil {
		return err
	}
	if g.sleepErr != nil {
		return g.sleepErr
	}
	if g.active != nil {
		g.active.end(nil)
		g.active = nil
	}
	g.sleptOK = true
	return nil
}

func (g *SleepGuard) Status(ctx context.Context) (contracts.GuardStatus, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return contracts.GuardStatus{}, err
	}
	if g.status != nil || g.statusErr != nil {
		if g.status == nil {
			return contracts.GuardStatus{}, g.statusErr
		}
		return *g.status, g.statusErr
	}
	st := contracts.GuardStatus{Backstop: contracts.BackstopIdle}
	if g.active != nil && !g.active.Ended() {
		st.SleepDisabled = true
		st.Backstop = contracts.BackstopArmed
		st.Lease = &contracts.LeaseInfo{
			ID: g.active.id, TTLS: int(g.active.ttl / time.Second), Expires: g.active.Expires(),
		}
	}
	return st, nil
}

// Acquires returns recorded Acquire calls.
func (g *SleepGuard) Acquires() []AcquireCall {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.acquires)
}

// Sleeps returns the reasons of recorded SleepNow calls (including
// failed ones).
func (g *SleepGuard) Sleeps() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.sleeps)
}

// Slept reports whether the last SleepNow succeeded and no lease was
// acquired since.
func (g *SleepGuard) Slept() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.sleptOK
}

// Leases returns every lease handed out, oldest first.
func (g *SleepGuard) Leases() []*Lease {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.leases)
}

// Active returns the active lease, or nil.
func (g *SleepGuard) Active() *Lease {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.active == nil || g.active.Ended() {
		return nil
	}
	return g.active
}

// Lease is a fake contracts.Lease. Use Lose to simulate a dropped
// connection, expiry or backstop.
type Lease struct {
	id    string
	ttl   time.Duration
	clock contracts.Clock

	mu        sync.Mutex
	expires   time.Time
	renews    int
	releases  int
	renewErrs []error
	ended     bool
	lost      chan error
}

var _ contracts.Lease = (*Lease)(nil)

func (l *Lease) ID() string { return l.id }

// TTL returns the TTL passed to Acquire.
func (l *Lease) TTL() time.Duration { return l.ttl }

func (l *Lease) Expires() time.Time {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.expires
}

func (l *Lease) Renew(ctx context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.renews++
	if l.ended {
		return fmt.Errorf("fakes: renew %s: %w", l.id, contracts.ErrLeaseLost)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(l.renewErrs) > 0 {
		err := l.renewErrs[0]
		l.renewErrs = l.renewErrs[1:]
		return err
	}
	l.expires = l.clock.Now().Add(l.ttl)
	return nil
}

func (l *Lease) Release(ctx context.Context) error {
	l.mu.Lock()
	l.releases++
	l.mu.Unlock()
	l.end(nil)
	return nil
}

func (l *Lease) Lost() <-chan error { return l.lost }

// Lose ends the lease as lost: Lost receives err (wrapped to match
// contracts.ErrLeaseLost if it doesn't already) and closes; later Renew
// calls fail. No-op if the lease already ended.
func (l *Lease) Lose(err error) {
	if err == nil {
		err = contracts.ErrLeaseLost
	} else if !errors.Is(err, contracts.ErrLeaseLost) {
		err = fmt.Errorf("%w: %w", contracts.ErrLeaseLost, err)
	}
	l.end(err)
}

// FailRenew makes the next Renew calls fail, one error each, without
// ending the lease (pair with Lose to model a daemon rejection).
func (l *Lease) FailRenew(errs ...error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.renewErrs = append(l.renewErrs, errs...)
}

// Renews returns the number of Renew calls.
func (l *Lease) Renews() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.renews
}

// Releases returns the number of Release calls.
func (l *Lease) Releases() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.releases
}

// Ended reports whether the lease was released, slept, or lost.
func (l *Lease) Ended() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.ended
}

func (l *Lease) end(err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.ended {
		return
	}
	l.ended = true
	if err != nil {
		l.lost <- err
	}
	close(l.lost)
}
