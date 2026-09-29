// Package fakes provides deterministic, in-memory implementations of every
// contracts interface for tests and `backpack simulate`. All fakes are safe
// for concurrent use.
package fakes

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/ilias-t/bckp/internal/contracts"
)

// Clock is a manual clock. Time moves only through Advance or Set. Timers created by After fire when the clock reaches their
// deadline; each channel is buffered so firing never blocks, and receives
// the deadline time.
type Clock struct {
	mu      sync.Mutex
	now     time.Time
	timers  []*fakeTimer
	changed chan struct{} // closed and replaced whenever timers change
}

type fakeTimer struct {
	deadline time.Time
	ch       chan time.Time
}

var _ contracts.Clock = (*Clock)(nil)

// NewClock returns a clock set to start.
func NewClock(start time.Time) *Clock {
	return &Clock{now: start, changed: make(chan struct{})}
}

// Now returns the fake current time.
func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// After returns a channel that receives once the clock reaches now+d.
// d <= 0 fires immediately.
func (c *Clock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := make(chan time.Time, 1)
	if d <= 0 {
		ch <- c.now
		return ch
	}
	c.timers = append(c.timers, &fakeTimer{deadline: c.now.Add(d), ch: ch})
	c.notifyLocked()
	return ch
}

// Advance moves the clock forward by d and fires every timer whose
// deadline is reached, in deadline order.
func (c *Clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.setLocked(c.now.Add(d))
}

// Set moves the clock to t (never backwards) and fires due timers.
func (c *Clock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if t.After(c.now) {
		c.setLocked(t)
	}
}

// PendingTimers returns the number of unfired After timers.
func (c *Clock) PendingTimers() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.timers)
}

// BlockUntilTimers waits until at least n timers are pending (e.g. until
// the code under test has parked in its poll loop), or ctx ends.
func (c *Clock) BlockUntilTimers(ctx context.Context, n int) error {
	for {
		c.mu.Lock()
		if len(c.timers) >= n {
			c.mu.Unlock()
			return nil
		}
		ch := c.changed
		c.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (c *Clock) setLocked(t time.Time) {
	c.now = t
	sort.SliceStable(c.timers, func(i, j int) bool { return c.timers[i].deadline.Before(c.timers[j].deadline) })
	kept := c.timers[:0]
	fired := false
	for _, tm := range c.timers {
		if !tm.deadline.After(t) {
			tm.ch <- tm.deadline
			fired = true
			continue
		}
		kept = append(kept, tm)
	}
	c.timers = kept
	if fired {
		c.notifyLocked()
	}
}

func (c *Clock) notifyLocked() {
	close(c.changed)
	c.changed = make(chan struct{})
}
