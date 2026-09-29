package fakes

import (
	"context"
	"slices"
	"sync"

	"github.com/ilias-t/bckp/internal/contracts"
)

// Notifier records every Send. Sends that fail (via SetErr or FailNext)
// are recorded in Attempts but not in Sent.
type Notifier struct {
	mu       sync.Mutex
	sent     []contracts.Notification
	attempts []contracts.Notification
	err      error
	failNext []error
}

var _ contracts.Notifier = (*Notifier)(nil)

// NewNotifier returns an empty recording notifier.
func NewNotifier() *Notifier { return &Notifier{} }

func (n *Notifier) Send(ctx context.Context, msg contracts.Notification) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	msg.Tags = slices.Clone(msg.Tags)
	n.attempts = append(n.attempts, msg)
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(n.failNext) > 0 {
		err := n.failNext[0]
		n.failNext = n.failNext[1:]
		return err
	}
	if n.err != nil {
		return n.err
	}
	n.sent = append(n.sent, msg)
	return nil
}

// SetErr makes every Send fail with err until SetErr(nil).
func (n *Notifier) SetErr(err error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.err = err
}

// FailNext queues one failure per error, consumed by subsequent Sends.
func (n *Notifier) FailNext(errs ...error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.failNext = append(n.failNext, errs...)
}

// Sent returns successfully sent notifications in order.
func (n *Notifier) Sent() []contracts.Notification {
	n.mu.Lock()
	defer n.mu.Unlock()
	return slices.Clone(n.sent)
}

// Attempts returns every notification passed to Send, including failures.
func (n *Notifier) Attempts() []contracts.Notification {
	n.mu.Lock()
	defer n.mu.Unlock()
	return slices.Clone(n.attempts)
}

// Reset clears recorded notifications (not SetErr/FailNext).
func (n *Notifier) Reset() {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.sent, n.attempts = nil, nil
}
