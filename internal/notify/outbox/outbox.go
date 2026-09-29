// Package outbox wraps a Notifier with a disk-backed retry queue, so
// notifications raised during a network outage are delivered once it ends.
package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ilias-t/bckp/internal/contracts"
)

// DefaultMax is how many messages the outbox keeps; older ones are dropped.
const DefaultMax = 100

const (
	fileExt   = ".json"
	tmpPrefix = ".tmp-"
)

// Outbox is a contracts.Notifier and contracts.Outbox that persists
// messages the inner notifier could not deliver, one JSON file per message
// in dir.
type Outbox struct {
	dir   string
	inner contracts.Notifier
	clock contracts.Clock
	max   int

	mu      sync.Mutex // guards the directory and seq
	seq     uint64
	flushMu sync.Mutex // serializes Flush
}

var (
	_ contracts.Notifier = (*Outbox)(nil)
	_ contracts.Outbox   = (*Outbox)(nil)
)

// entry is the on-disk form of one queued message.
type entry struct {
	Notification contracts.Notification `json:"notification"`
	QueuedAt     time.Time              `json:"queued_at"`
}

// New returns an Outbox storing messages in dir (created 0700 on first
// use). Leftover temp files from an interrupted write are removed.
func New(dir string, inner contracts.Notifier, clock contracts.Clock) *Outbox {
	if tmps, _ := filepath.Glob(filepath.Join(dir, tmpPrefix+"*")); len(tmps) > 0 {
		for _, p := range tmps {
			_ = os.Remove(p)
		}
	}
	return &Outbox{dir: dir, inner: inner, clock: clock, max: DefaultMax}
}

// Send delivers n through the inner notifier.
//   - Success: returns nil, then opportunistically flushes older queued
//     messages (so they arrive after n).
//   - Failure: persists n and returns an error wrapping both
//     contracts.ErrQueued and the inner error. If persisting fails too,
//     the error does not wrap contracts.ErrQueued.
//
// Invalid notifications are rejected without being queued.
func (o *Outbox) Send(ctx context.Context, n contracts.Notification) error {
	if err := n.Validate(); err != nil {
		return err
	}
	err := o.inner.Send(ctx, n)
	if err == nil {
		if o.Pending() > 0 && o.flushMu.TryLock() {
			_ = o.flushLocked(ctx)
			o.flushMu.Unlock()
		}
		return nil
	}
	if qerr := o.enqueue(n); qerr != nil {
		return fmt.Errorf("outbox: delivery failed (%w) and queueing failed: %v", err, qerr)
	}
	return fmt.Errorf("outbox: %w: %w", contracts.ErrQueued, err)
}

// Flush retries queued messages oldest first and stops at the first
// failure, so order is preserved. Unreadable files are dropped. Delivered
// messages carry a "(queued HH:MM)" note in the body.
func (o *Outbox) Flush(ctx context.Context) error {
	o.flushMu.Lock()
	defer o.flushMu.Unlock()
	return o.flushLocked(ctx)
}

func (o *Outbox) flushLocked(ctx context.Context) error {
	names, err := o.list()
	if err != nil {
		return err
	}
	for i, name := range names {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("outbox: flush: %d pending: %w", len(names)-i, err)
		}
		e, err := o.read(name)
		if errors.Is(err, fs.ErrNotExist) {
			continue // dropped by the cap meanwhile
		}
		if err != nil {
			o.remove(name) // corrupt: never deliverable
			continue
		}
		msg := e.Notification
		msg.Body = annotate(msg.Body, e.QueuedAt)
		if err := o.inner.Send(ctx, msg); err != nil {
			return fmt.Errorf("outbox: flush: %d pending: %w", len(names)-i, err)
		}
		o.remove(name)
	}
	return nil
}

// Pending returns the number of queued messages.
func (o *Outbox) Pending() int {
	names, _ := o.list()
	return len(names)
}

func annotate(body string, queuedAt time.Time) string {
	note := fmt.Sprintf("(queued %s)", queuedAt.Format("15:04"))
	if body == "" {
		return note
	}
	return body + "\n\n" + note
}

// enqueue writes n atomically (temp file + rename) and enforces the cap.
func (o *Outbox) enqueue(n contracts.Notification) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if err := os.MkdirAll(o.dir, 0o700); err != nil {
		return err
	}
	now := o.clock.Now()
	data, err := json.Marshal(entry{Notification: n, QueuedAt: now})
	if err != nil {
		return err
	}
	var final string
	for {
		o.seq++
		// Fixed-width fields: lexical order is queue order.
		final = filepath.Join(o.dir, fmt.Sprintf("%020d-%06d%s", now.UnixNano(), o.seq%1_000_000, fileExt))
		if _, err := os.Lstat(final); errors.Is(err, fs.ErrNotExist) {
			break
		}
	}
	if err := writeAtomic(o.dir, final, data); err != nil {
		return err
	}
	return o.trimLocked()
}

func writeAtomic(dir, final string, data []byte) error {
	f, err := os.CreateTemp(dir, tmpPrefix+"*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, final)
	}
	if err != nil {
		_ = os.Remove(tmp)
	}
	return err
}

func (o *Outbox) trimLocked() error {
	names, err := o.list()
	if err != nil {
		return err
	}
	for _, name := range names[:max(0, len(names)-o.max)] {
		o.remove(name)
	}
	return nil
}

// list returns queued file names, oldest first.
func (o *Outbox) list() ([]string, error) {
	ents, err := os.ReadDir(o.dir) // sorted by name
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("outbox: %w", err)
	}
	var names []string
	for _, e := range ents {
		if n := e.Name(); e.Type().IsRegular() && strings.HasSuffix(n, fileExt) && !strings.HasPrefix(n, ".") {
			names = append(names, n)
		}
	}
	return names, nil
}

func (o *Outbox) read(name string) (entry, error) {
	var e entry
	data, err := os.ReadFile(filepath.Join(o.dir, name))
	if err != nil {
		return e, err
	}
	if err := json.Unmarshal(data, &e); err != nil {
		return e, err
	}
	return e, e.Notification.Validate()
}

func (o *Outbox) remove(name string) {
	_ = os.Remove(filepath.Join(o.dir, name))
}
