// Package macos posts macOS Notification Center banners via osascript.
package macos

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/ilias-t/bckp/internal/contracts"
	"github.com/ilias-t/bckp/internal/paths"
)

// BinOsascript is osascript's absolute path.
const BinOsascript = paths.BinOsascript

// Timeout bounds one osascript run.
const Timeout = 10 * time.Second

// DefaultTitle is used when a notification has no title.
const DefaultTitle = "backpack"

// script reads the title and body from argv, so user text never becomes
// AppleScript source and needs no escaping.
var script = []string{
	"-e", "on run argv",
	"-e", "display notification (item 2 of argv) with title (item 1 of argv)",
	"-e", "end run",
}

// Args returns the osascript argv for a title and body. "--" keeps a
// title or body starting with "-" from being read as an option.
func Args(title, body string) []string {
	return append(append([]string(nil), script...), "--", title, body)
}

// Notifier posts via osascript.
type Notifier struct {
	exec contracts.Exec
}

var _ contracts.Notifier = (*Notifier)(nil)

// New returns a Notifier that runs osascript through exec.
func New(exec contracts.Exec) contracts.Notifier {
	return &Notifier{exec: exec}
}

// Send displays n as a banner. Priority and tags are not shown.
func (m *Notifier) Send(ctx context.Context, n contracts.Notification) error {
	if err := n.Validate(); err != nil {
		return err
	}
	title := clean(n.Title)
	if title == "" {
		title = DefaultTitle
	}
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	if _, err := m.exec.Run(ctx, BinOsascript, Args(title, clean(n.Body))...); err != nil {
		return fmt.Errorf("macos notify: %w", err)
	}
	return nil
}

// clean drops NUL bytes, which cannot be passed in argv.
func clean(s string) string {
	return strings.ReplaceAll(s, "\x00", "")
}
