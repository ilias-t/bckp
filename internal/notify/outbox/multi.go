package outbox

import (
	"context"
	"errors"
	"sync"

	"github.com/ilias-t/bckp/internal/contracts"
)

// Multi fans a notification out to every non-nil notifier concurrently.
// Send returns errors.Join of the individual failures (nil if all
// succeeded), so errors.Is(err, contracts.ErrQueued) still detects a
// queued delivery. With no notifiers, Send only validates. The result is a
// contracts.Outbox for the members that are one.
func Multi(notifiers ...contracts.Notifier) contracts.Notifier {
	var m multi
	for _, n := range notifiers {
		if n != nil {
			m = append(m, n)
		}
	}
	if len(m) == 1 {
		return m[0]
	}
	return m
}

type multi []contracts.Notifier

func (m multi) Send(ctx context.Context, n contracts.Notification) error {
	if len(m) == 0 {
		return n.Validate()
	}
	errs := make([]error, len(m))
	var wg sync.WaitGroup
	for i, x := range m {
		wg.Go(func() { errs[i] = x.Send(ctx, n) })
	}
	wg.Wait()
	return errors.Join(errs...)
}

// Pending sums the members' outboxes.
func (m multi) Pending() int {
	n := 0
	for _, x := range m {
		if ob, ok := x.(contracts.Outbox); ok {
			n += ob.Pending()
		}
	}
	return n
}

// Flush flushes the members' outboxes.
func (m multi) Flush(ctx context.Context) error {
	var errs []error
	for _, x := range m {
		if ob, ok := x.(contracts.Outbox); ok {
			errs = append(errs, ob.Flush(ctx))
		}
	}
	return errors.Join(errs...)
}
