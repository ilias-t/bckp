package engine

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ilias-t/bckp/internal/config"
	"github.com/ilias-t/bckp/internal/contracts"
)

// hookOutbox is a notifier with an outbox (contracts.Outbox) driven by
// funcs.
type hookOutbox struct {
	contracts.Notifier
	pending func() int
	flush   func(context.Context) error
}

func (o hookOutbox) Pending() int                    { return o.pending() }
func (o hookOutbox) Flush(ctx context.Context) error { return o.flush(ctx) }

// TestOutboxRetriedWhileOnline: queued notifications are retried from the
// poll loop while the network is online, at most once per 30s, without
// blocking the loop, and not while offline.
func TestOutboxRetriedWhileOnline(t *testing.T) {
	h := newHarness(t, nil)
	var pending, calls atomic.Int32
	entered := make(chan struct{}, 8)
	release := make(chan struct{})
	h.deps.Notifier = hookOutbox{Notifier: h.notifier, pending: func() int { return int(pending.Load()) },
		flush: func(ctx context.Context) error {
			calls.Add(1)
			entered <- struct{}{}
			select {
			case <-release:
			case <-ctx.Done():
			}
			pending.Store(0)
			return nil
		}}
	h.start(Options{})
	h.polls(2)
	if calls.Load() != 0 {
		t.Fatal("flushed with nothing pending")
	}

	pending.Store(1)
	h.polls(1)
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("no flush while online with messages waiting")
	}
	// The flush is stuck; polls go on regardless (a blocked loop would
	// not park, failing the harness). One poll: 45s, before the next
	// request is due at 60s.
	h.polls(1)
	close(release)
	deadline := time.Now().Add(5 * time.Second)
	for pending.Load() != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if pending.Load() != 0 {
		t.Fatal("flush never finished")
	}

	// Offline: no retries.
	h.net.PushSnapshot(offline())
	h.polls(1)
	pending.Store(1)
	before := calls.Load()
	h.polls(6)
	if calls.Load() != before {
		t.Fatal("flushed while offline")
	}
	h.do(func() { _ = h.e.Stop(context.Background(), nil) })
	h.wait()
	if got := calls.Load(); got != 1 {
		t.Fatalf("FlushOutbox calls = %d, want 1", got)
	}
}

// TestOutboxFlushRateLimited: with messages that keep failing, the loop
// asks at most once per 30s.
func TestOutboxFlushRateLimited(t *testing.T) {
	h := newHarness(t, nil)
	var calls atomic.Int32
	h.deps.Notifier = hookOutbox{Notifier: h.notifier, pending: func() int { return 1 },
		flush: func(context.Context) error { calls.Add(1); return context.DeadlineExceeded }}
	h.start(Options{}) // poll 1 at 0s asks
	h.polls(8)         // 15s .. 120s: asks at 30s, 60s, 90s, 120s
	// The sender runs the flushes asynchronously.
	deadline := time.Now().Add(5 * time.Second)
	for calls.Load() < 5 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	h.do(func() { _ = h.e.Stop(context.Background(), nil) })
	h.wait()
	if got := calls.Load(); got != 5 {
		t.Fatalf("FlushOutbox calls = %d, want 5 (every 30s over 2m)", got)
	}
}

// TestOutboxRetriedWithoutNetworkWatch: with the network feature off
// nothing says whether the Mac is online, so the outbox is retried on its
// interval anyway.
func TestOutboxRetriedWithoutNetworkWatch(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.Network.Watchdog = false })
	h.deps.Network = nil
	var calls atomic.Int32
	h.deps.Notifier = hookOutbox{Notifier: h.notifier, pending: func() int { return 1 },
		flush: func(context.Context) error { calls.Add(1); return context.DeadlineExceeded }}
	h.start(Options{})
	h.polls(2)
	deadline := time.Now().Add(5 * time.Second)
	for calls.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	h.cancel()
	h.wait()
	if got := calls.Load(); got != 2 {
		t.Fatalf("Flush calls = %d, want 2 (at 0s and 30s)", got)
	}
}

// TestBackOnlineHeldOnePoll: "back online" goes out one poll after the
// link comes back (a link seconds old often times out the first request),
// even if it dropped again, and at the session's end if that is sooner.
func TestBackOnlineHeldOnePoll(t *testing.T) {
	h := newHarness(t, nil)
	h.start(Options{})
	h.net.PushSnapshot(offline())
	h.polls(3)
	h.net.PushSnapshot(online())
	h.polls(1)
	if h.notified("Mac back online") {
		t.Fatal("sent in the poll that saw the link back")
	}
	h.net.PushSnapshot(offline())
	h.polls(1)
	if !h.notified("Mac back online after 45s via hotspot (USB)") {
		t.Fatalf("not sent on the next poll: %q", h.titles())
	}

	h = newHarness(t, nil)
	h.start(Options{})
	h.net.PushSnapshot(offline())
	h.polls(3)
	h.net.PushSnapshot(online())
	h.polls(1)
	h.do(func() { _ = h.e.Stop(context.Background(), nil) })
	h.wait()
	if got := h.titles(); len(got) < 2 || got[len(got)-2] != "Mac back online after 45s via hotspot (USB)" || got[len(got)-1] != "backpack off" {
		t.Fatalf("held notification lost at the end: %q", got)
	}
}
