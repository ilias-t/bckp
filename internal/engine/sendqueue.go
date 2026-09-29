package engine

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/ilias-t/bckp/internal/contracts"
)

const (
	sendQueueSize = 64
	sendTimeout   = 20 * time.Second
	// outboxFlushInterval rate-limits outbox retries from the poll loop.
	outboxFlushInterval = 30 * time.Second
	// cutOffGrace bounds the wait for cancelled sends to return at the end
	// of a flush (each fails at once, so the outbox can keep it).
	cutOffGrace = time.Second
)

// sendQueue delivers notifications in order from one goroutine, so the
// loop only enqueues. Failures are logged and recorded; they never stop the
// engine (durable retry is the outbox wrapper's job).
type sendQueue struct {
	e  *Engine
	ch chan queued

	mu sync.Mutex
	// ctx is the parent of every send; flush cancels it at its deadline.
	ctx     context.Context
	cancel  context.CancelFunc
	nextID  uint64
	pending int
	waiters []chan struct{}
	closed  bool
	// noFlush: the session is ending; outbox flush requests are skipped so
	// the last notifications are not stuck behind one.
	noFlush bool
}

type queued struct {
	id uint64
	n  contracts.Notification
	// flush marks a request to retry the outbox (contracts.Outbox)
	// instead of a notification.
	flush bool
}

func newSendQueue(e *Engine) *sendQueue {
	q := &sendQueue{e: e, ch: make(chan queued, sendQueueSize)}
	q.ctx, q.cancel = context.WithCancel(context.Background())
	go q.run()
	return q
}

// enqueue records n as queued and hands it to the sender. It never blocks;
// when the queue is full n is recorded as failed.
func (q *sendQueue) enqueue(now time.Time, n contracts.Notification) {
	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		return
	}
	q.nextID++
	id := q.nextID
	q.pending++
	q.mu.Unlock()

	// Record before handing off, so the sender always finds the record.
	e := q.e
	e.mu.Lock()
	e.recs = append(e.recs, notifRecord{id: id, rec: contracts.NotificationRecord{
		Notification: n, At: now, Status: contracts.NotificationQueued,
	}})
	if len(e.recs) > contracts.MaxSnapshotNotifications {
		e.recs = slices.Delete(e.recs, 0, len(e.recs)-contracts.MaxSnapshotNotifications)
	}
	e.mu.Unlock()

	select {
	case q.ch <- queued{id: id, n: n}:
	default:
		q.record(queued{id: id, n: n}, errQueueFull)
		q.done()
	}
}

var errQueueFull = errors.New("send queue full")

// requestFlush asks the sender to retry the outbox once the notifications
// queued before it are sent. It never blocks: with the queue full the
// request is dropped (the next poll asks again). Flush requests are not
// counted as pending, so the exit flush never waits for one.
func (q *sendQueue) requestFlush() {
	q.mu.Lock()
	skip := q.closed || q.noFlush
	q.mu.Unlock()
	if skip {
		return
	}
	select {
	case q.ch <- queued{flush: true}:
	default:
	}
}

func (q *sendQueue) run() {
	for item := range q.ch {
		q.mu.Lock()
		base, skip := q.ctx, q.noFlush
		q.mu.Unlock()
		if item.flush {
			if !skip {
				q.flushOutbox(base)
			}
			continue
		}
		ctx, cancel := context.WithTimeout(base, sendTimeout)
		err := q.e.d.Notifier.Send(ctx, item.n)
		cancel()
		q.record(item, err)
		q.done()
	}
}

// done marks one queued notification as attempted and wakes flushers when
// none are left.
func (q *sendQueue) done() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.pending--
	if q.pending == 0 {
		for _, w := range q.waiters {
			close(w)
		}
		q.waiters = nil
	}
}

func (q *sendQueue) record(item queued, err error) {
	e := q.e
	queuedForRetry := errors.Is(err, contracts.ErrQueued)
	// Title and priority only, never the body. ntfy errors carry the URL
	// with the topic; the event log handler redacts it.
	class := string(item.n.Class)
	switch {
	case queuedForRetry:
		e.ev(slog.LevelWarn, "notify.queued", "title", item.n.Title, "priority", item.n.Priority, "class", class, "pushed", true,
			"err", err)
	case err != nil:
		e.ev(slog.LevelError, "notify.failed", "title", item.n.Title, "priority", item.n.Priority, "class", class, "pushed", true,
			"err", err)
	default:
		e.ev(slog.LevelInfo, "notify.sent", "title", item.n.Title, "priority", item.n.Priority, "class", class, "pushed", true)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if err != nil {
		e.notifyErr = err.Error()
	} else {
		e.notifyErr = ""
	}
	for i := range e.recs {
		if e.recs[i].id != item.id {
			continue
		}
		r := &e.recs[i].rec
		switch {
		case queuedForRetry:
			r.Status, r.Err = contracts.NotificationQueued, err.Error()
		case err != nil:
			r.Status, r.Err = contracts.NotificationFailed, err.Error()
		default:
			r.Status, r.Err = contracts.NotificationSent, ""
		}
	}
	e.republishLocked()
}

// flushOutbox retries the outbox (on the sender goroutine) and logs what it
// delivered.
func (q *sendQueue) flushOutbox(base context.Context) {
	e := q.e
	if e.ob == nil {
		return
	}
	before := e.ob.Pending()
	if before == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(base, sendTimeout)
	err := e.ob.Flush(ctx)
	cancel()
	after := e.ob.Pending()
	if err != nil {
		e.ev(slog.LevelDebug, "notify.outbox_flush", "result", "failed", "delivered", max(0, before-after),
			"pending", after, "err", err)
	} else {
		e.ev(slog.LevelInfo, "notify.outbox_flush", "result", "ok", "delivered", max(0, before-after), "pending", after)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.republishLocked()
}

// flush waits up to timeout until every queued notification was attempted.
// Past it, the in-flight send and those not yet attempted are cancelled:
// each fails at once, and an outbox notifier keeps it for a later retry
// instead of losing it. Later sends get a fresh context. It returns how
// many notifications were cut off.
func (q *sendQueue) flush(timeout time.Duration) int {
	if q.wait(timeout) {
		return 0
	}
	q.mu.Lock()
	cut := q.pending
	q.cancel()
	q.mu.Unlock()
	q.wait(cutOffGrace)
	q.mu.Lock()
	q.ctx, q.cancel = context.WithCancel(context.Background())
	q.mu.Unlock()
	return cut
}

// wait reports whether every queued notification was attempted within
// timeout.
func (q *sendQueue) wait(timeout time.Duration) bool {
	q.mu.Lock()
	if q.pending == 0 {
		q.mu.Unlock()
		return true
	}
	w := make(chan struct{})
	q.waiters = append(q.waiters, w)
	q.mu.Unlock()
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case <-w:
		return true
	case <-t.C:
		return false
	}
}

// stopFlushes drops pending and future outbox flush requests (shutdown).
func (q *sendQueue) stopFlushes() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.noFlush = true
}

// close stops accepting notifications and cancels any send still running;
// the sender exits once the queue is drained.
func (q *sendQueue) close() {
	q.mu.Lock()
	defer q.mu.Unlock()
	if !q.closed {
		q.closed = true
		close(q.ch)
		q.cancel()
	}
}
