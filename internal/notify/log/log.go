// Package log is a Notifier that writes notifications to a slog.Logger.
package log

import (
	"context"
	"log/slog"

	"github.com/ilias-t/bckp/internal/contracts"
)

// Notifier logs each notification at Info.
type Notifier struct {
	logger *slog.Logger
}

var _ contracts.Notifier = (*Notifier)(nil)

// New returns a Notifier that logs to logger (slog.Default() when nil).
func New(logger *slog.Logger) contracts.Notifier {
	if logger == nil {
		logger = slog.Default()
	}
	return &Notifier{logger: logger}
}

// Send logs n. It fails only for an invalid notification.
func (l *Notifier) Send(ctx context.Context, n contracts.Notification) error {
	if err := n.Validate(); err != nil {
		return err
	}
	l.logger.InfoContext(ctx, "notification",
		slog.Int("priority", n.Priority),
		slog.String("title", n.Title),
		slog.String("body", n.Body),
		slog.Any("tags", n.Tags),
	)
	return nil
}
