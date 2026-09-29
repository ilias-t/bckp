// Package engineclient implements contracts.EngineClient over the engine
// protocol (internal/proto, engine.go): newline-delimited JSON on the
// engine's Unix socket, one request per connection.
package engineclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"syscall"
	"time"

	"github.com/ilias-t/bckp/internal/contracts"
	"github.com/ilias-t/bckp/internal/proto"
)

// DefaultTimeout bounds a single request/response exchange when ctx has
// no earlier deadline. Subscribe streams are not bounded by it.
const DefaultTimeout = 10 * time.Second

// Client talks to the engine at a socket path. The zero value is not
// usable; use New.
type Client struct {
	socket string
}

var _ contracts.EngineClient = (*Client)(nil)

// New returns a client for the engine socket at socketPath. It does not
// connect until a method is called.
func New(socketPath string) contracts.EngineClient {
	return &Client{socket: socketPath}
}

// dial connects to the engine. A missing socket file or a refused
// connection (stale socket, engine gone) maps to ErrEngineNotRunning.
func (c *Client) dial(ctx context.Context) (net.Conn, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", c.socket)
	if err == nil {
		return conn, nil
	}
	if errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ECONNREFUSED) {
		return nil, fmt.Errorf("%w (%s)", contracts.ErrEngineNotRunning, c.socket)
	}
	return nil, fmt.Errorf("engine: dial %s: %w", c.socket, err)
}

// roundTrip sends one request and reads one response.
func (c *Client) roundTrip(ctx context.Context, req proto.EngineRequest) (proto.EngineResponse, error) {
	if err := req.Validate(); err != nil {
		return proto.EngineResponse{}, err
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, DefaultTimeout)
		defer cancel()
	}
	conn, err := c.dial(ctx)
	if err != nil {
		return proto.EngineResponse{}, err
	}
	defer conn.Close()
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Unix(1, 0)) })
	defer stop()

	if err := proto.WriteLine(conn, req); err != nil {
		return proto.EngineResponse{}, c.ioErr(ctx, "write", err)
	}
	var resp proto.EngineResponse
	if err := proto.NewLineReader(conn, proto.MaxEngineResponseBytes).Decode(&resp, false); err != nil {
		return proto.EngineResponse{}, c.ioErr(ctx, "read", err)
	}
	if err := resp.Err(); err != nil {
		return resp, err
	}
	return resp, nil
}

func (c *Client) ioErr(ctx context.Context, what string, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return fmt.Errorf("engine: %s: %w", what, ctxErr)
	}
	// The only I/O deadlines we set come from ctx; the socket timer can
	// fire a moment before ctx's own timer marks it done.
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return fmt.Errorf("engine: %s: %w", what, context.DeadlineExceeded)
	}
	if errors.Is(err, io.EOF) {
		return fmt.Errorf("engine: %s: connection closed without a response", what)
	}
	return fmt.Errorf("engine: %s: %w", what, err)
}

func snapshotOf(resp proto.EngineResponse) (contracts.Snapshot, error) {
	if resp.Snapshot == nil {
		return contracts.Snapshot{}, errors.New("engine: response has no snapshot")
	}
	return *resp.Snapshot, nil
}

// Status returns the engine's current snapshot.
func (c *Client) Status(ctx context.Context) (contracts.Snapshot, error) {
	resp, err := c.roundTrip(ctx, proto.EngineRequest{Op: proto.OpEngineStatus})
	if err != nil {
		return contracts.Snapshot{}, err
	}
	return snapshotOf(resp)
}

// Stop ends the session. sleep is tri-state; see proto.EngineRequest.
func (c *Client) Stop(ctx context.Context, sleep *bool) error {
	_, err := c.roundTrip(ctx, proto.EngineRequest{Op: proto.OpEngineStop, Sleep: sleep})
	return err
}

// Extend adds minutes, or switches the session to forever.
func (c *Client) Extend(ctx context.Context, minutes int, forever bool) (contracts.Snapshot, error) {
	req := proto.EngineRequest{Op: proto.OpEngineExtend, Minutes: minutes, Forever: forever}
	if forever {
		req.Minutes = 0
	}
	resp, err := c.roundTrip(ctx, req)
	if err != nil {
		return contracts.Snapshot{}, err
	}
	return snapshotOf(resp)
}

// AbortSleep cancels a pending abortable sleep without adding time.
func (c *Client) AbortSleep(ctx context.Context) (contracts.Snapshot, error) {
	resp, err := c.roundTrip(ctx, proto.EngineRequest{Op: proto.OpEngineAbortSleep})
	if err != nil {
		return contracts.Snapshot{}, err
	}
	return snapshotOf(resp)
}

// Subscribe streams snapshots until the session ends (a Done line), ctx
// is cancelled, or the connection drops; then the channel is closed.
func (c *Client) Subscribe(ctx context.Context) (<-chan contracts.Snapshot, error) {
	conn, err := c.dial(ctx)
	if err != nil {
		return nil, err
	}
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	if err := proto.WriteLine(conn, proto.EngineRequest{Op: proto.OpEngineSubscribe}); err != nil {
		stop()
		conn.Close()
		return nil, c.ioErr(ctx, "write", err)
	}
	lr := proto.NewLineReader(conn, proto.MaxEngineResponseBytes)
	// Read the first line synchronously so a refused subscription is an
	// error rather than an empty stream.
	var first proto.EngineResponse
	if err := lr.Decode(&first, false); err != nil {
		stop()
		conn.Close()
		return nil, c.ioErr(ctx, "read", err)
	}
	if err := first.Err(); err != nil {
		stop()
		conn.Close()
		return nil, err
	}

	ch := make(chan contracts.Snapshot, 1)
	go func() {
		defer close(ch)
		defer stop()
		defer conn.Close()
		resp := first
		for {
			if resp.Snapshot != nil {
				select {
				case ch <- *resp.Snapshot:
				case <-ctx.Done():
					return
				}
			}
			if resp.Done || !resp.OK {
				return
			}
			resp = proto.EngineResponse{}
			if err := lr.Decode(&resp, false); err != nil {
				return
			}
		}
	}()
	return ch, nil
}
