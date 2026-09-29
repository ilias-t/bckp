// Package ntfy publishes notifications to an ntfy server
// (https://docs.ntfy.sh/publish/#publish-as-json).
package ntfy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ilias-t/bckp/internal/contracts"
)

// Timeout bounds one publish.
const Timeout = 10 * time.Second

// Notifier publishes to one topic.
type Notifier struct {
	endpoint string
	topic    string
	client   *http.Client
}

var _ contracts.Notifier = (*Notifier)(nil)

// New returns a Notifier for server (e.g. "https://ntfy.sh") and topic. A
// nil client gets one with a 10s timeout. Messages are POSTed as JSON to
// the server root with the topic in the body, so the topic never appears
// in a URL or in error messages.
func New(server, topic string, client *http.Client) contracts.Notifier {
	if client == nil {
		client = &http.Client{Timeout: Timeout}
	}
	return &Notifier{endpoint: strings.TrimRight(server, "/") + "/", topic: topic, client: client}
}

type message struct {
	Topic    string   `json:"topic"`
	Title    string   `json:"title,omitempty"`
	Message  string   `json:"message"`
	Priority int      `json:"priority,omitempty"`
	Tags     []string `json:"tags,omitempty"`
}

// Send publishes n and returns an error unless the server answers 2xx.
func (c *Notifier) Send(ctx context.Context, n contracts.Notification) error {
	if err := n.Validate(); err != nil {
		return err
	}
	if c.topic == "" {
		return fmt.Errorf("ntfy: no topic configured")
	}
	body, err := json.Marshal(message{
		Topic:    c.topic,
		Title:    n.Title,
		Message:  n.Body,
		Priority: n.Priority, // 0 is omitted: ntfy's default is 3
		Tags:     n.Tags,
	})
	if err != nil {
		return fmt.Errorf("ntfy: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("ntfy: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		if ue, ok := err.(*url.Error); ok {
			err = ue.Err // drop the "Post <url>:" prefix
		}
		return fmt.Errorf("ntfy: publish: %w", err)
	}
	defer resp.Body.Close()
	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("ntfy: publish: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(snippet)))
	}
	return nil
}
