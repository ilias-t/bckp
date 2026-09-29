package fakes

import (
	"context"
	"slices"
	"sync"

	"github.com/ilias-t/bckp/internal/contracts"
)

// ExtendCall records one EngineClient.Extend.
type ExtendCall struct {
	Minutes int
	Forever bool
}

// SubscribeBuffer is the per-subscriber channel buffer. When a subscriber
// falls this far behind, further snapshots are dropped for it (like the
// real engine dropping slow subscribers, but without closing).
const SubscribeBuffer = 64

// EngineClient is a fake contracts.EngineClient for CLI and TUI tests. Push
// snapshots to drive subscribers; End or Drop to finish the stream.
type EngineClient struct {
	mu        sync.Mutex
	snap      contracts.Snapshot
	running   bool
	subs      map[*engineSub]struct{}
	stops     []*bool
	extends   []ExtendCall
	aborts    int
	abortErr  error
	statusErr error
	stopErr   error
	extendErr error
}

type engineSub struct {
	ch     chan contracts.Snapshot
	done   chan struct{} // closed with ch; stops the ctx watcher
	closed bool
}

var _ contracts.EngineClient = (*EngineClient)(nil)

// NewEngineClient returns a running fake engine with an initial snapshot.
func NewEngineClient(initial contracts.Snapshot) *EngineClient {
	return &EngineClient{snap: initial, running: true, subs: map[*engineSub]struct{}{}}
}

// SetErrs sets errors returned by Status, Stop and Extend (nil clears).
func (e *EngineClient) SetErrs(status, stop, extend error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.statusErr, e.stopErr, e.extendErr = status, stop, extend
}

// Push replaces the current snapshot and sends it to every subscriber.
func (e *EngineClient) Push(s contracts.Snapshot) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.snap = s
	for sub := range e.subs {
		select {
		case sub.ch <- s:
		default: // subscriber too slow; drop this snapshot for it
		}
	}
}

// End finishes the session: sends a final snapshot with Session.Ended and
// reason set, closes all subscriptions, and makes further calls return
// contracts.ErrEngineNotRunning.
func (e *EngineClient) End(reason contracts.EndReason) {
	e.mu.Lock()
	final := e.snap
	final.Session.Ended = true
	final.Session.EndReason = reason
	e.snap = final
	e.mu.Unlock()
	e.Push(final)
	e.Drop()
}

// Drop closes all subscriptions without a final snapshot (a lost
// connection) and marks the engine as not running.
func (e *EngineClient) Drop() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.running = false
	for sub := range e.subs {
		e.closeLocked(sub)
	}
}

func (e *EngineClient) closeLocked(sub *engineSub) {
	if !sub.closed {
		sub.closed = true
		close(sub.ch)
		close(sub.done)
	}
	delete(e.subs, sub)
}

func (e *EngineClient) Status(ctx context.Context) (contracts.Snapshot, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.checkLocked(ctx, e.statusErr); err != nil {
		return contracts.Snapshot{}, err
	}
	return e.snap, nil
}

func (e *EngineClient) Stop(ctx context.Context, sleep *bool) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	var rec *bool
	if sleep != nil {
		v := *sleep
		rec = &v
	}
	e.stops = append(e.stops, rec)
	return e.checkLocked(ctx, e.stopErr)
}

func (e *EngineClient) Extend(ctx context.Context, minutes int, forever bool) (contracts.Snapshot, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.extends = append(e.extends, ExtendCall{Minutes: minutes, Forever: forever})
	if err := e.checkLocked(ctx, e.extendErr); err != nil {
		return contracts.Snapshot{}, err
	}
	return e.snap, nil
}

// AbortSleep models the engine: ErrNoSleepPending without a pending
// sleep, ErrNotAbortable for a critical one, else it clears SleepPending
// (nothing else changes) and returns the snapshot. SetAbortErr injects an
// error instead.
func (e *EngineClient) AbortSleep(ctx context.Context) (contracts.Snapshot, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.aborts++
	if err := e.checkLocked(ctx, e.abortErr); err != nil {
		return contracts.Snapshot{}, err
	}
	switch p := e.snap.SleepPending; {
	case p == nil:
		return contracts.Snapshot{}, contracts.ErrNoSleepPending
	case !p.Abortable:
		return contracts.Snapshot{}, contracts.ErrNotAbortable
	}
	e.snap.SleepPending = nil
	if e.snap.Thermal.FSM == contracts.FSMSleepPending {
		e.snap.Thermal.FSM = contracts.FSMHot
	}
	return e.snap, nil
}

// SetAbortErr sets the error AbortSleep returns (nil clears).
func (e *EngineClient) SetAbortErr(err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.abortErr = err
}

// Aborts returns how many times AbortSleep was called.
func (e *EngineClient) Aborts() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.aborts
}

// Subscribe delivers the current snapshot first, then every Push. The
// channel closes on End, Drop, or ctx cancellation.
func (e *EngineClient) Subscribe(ctx context.Context) (<-chan contracts.Snapshot, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.checkLocked(ctx, nil); err != nil {
		return nil, err
	}
	sub := &engineSub{ch: make(chan contracts.Snapshot, SubscribeBuffer), done: make(chan struct{})}
	sub.ch <- e.snap
	e.subs[sub] = struct{}{}
	go func() {
		select {
		case <-ctx.Done():
		case <-sub.done:
			return
		}
		e.mu.Lock()
		defer e.mu.Unlock()
		e.closeLocked(sub)
	}()
	return sub.ch, nil
}

func (e *EngineClient) checkLocked(ctx context.Context, injected error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !e.running {
		return contracts.ErrEngineNotRunning
	}
	return injected
}

// Stops returns the sleep argument of each Stop call (copied; nil = unset).
func (e *EngineClient) Stops() []*bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.stops)
}

// Extends returns recorded Extend calls.
func (e *EngineClient) Extends() []ExtendCall {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.extends)
}

// Subscribers returns the number of open subscriptions.
func (e *EngineClient) Subscribers() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.subs)
}
