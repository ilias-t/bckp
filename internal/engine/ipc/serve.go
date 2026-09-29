// Package ipc serves the engine protocol (internal/proto, engine.go) on a
// listener, typically the unix socket at paths.User.EngineSocket. It is the
// server side of what internal/engineclient (contracts.EngineClient)
// speaks.
//
// One request per connection: the client writes one EngineRequest line;
// the server answers one EngineResponse line and closes, except for
// subscribe, which streams snapshots until the session ends (the last line
// has done=true), the client disconnects, a write blocks longer than
// proto.SubscribeWriteTimeout, or Serve's ctx ends.
package ipc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/ilias-t/bckp/internal/contracts"
	"github.com/ilias-t/bckp/internal/proto"
)

// Controller is the engine surface the protocol drives. *engine.Engine
// satisfies it.
type Controller interface {
	Snapshot() contracts.Snapshot
	Stop(ctx context.Context, sleep *bool) error
	Extend(ctx context.Context, minutes int, forever bool) (contracts.Snapshot, error)
	AbortSleep(ctx context.Context) (contracts.Snapshot, error)
	Subscribe(ctx context.Context) <-chan contracts.Snapshot
}

const (
	// RequestReadTimeout bounds how long a client may take to send its
	// request line.
	RequestReadTimeout = 5 * time.Second
	// MaxConns caps concurrent connections; extra ones get a busy answer.
	MaxConns = 32
)

// ShutdownGrace bounds how long Serve, once ctx ends, waits for in-flight
// status/stop/extend requests before cancelling them. A var so tests can
// shorten it.
var ShutdownGrace = 5 * time.Second

// Serve accepts connections on ln until ctx ends or ln is closed, then
// waits for in-flight connections to finish and returns nil. It closes ln
// when ctx ends. A non-recoverable Accept error is returned.
//
// On shutdown, subscribe streams end at once, but status/stop/extend
// requests already in flight keep a live ctx for up to ShutdownGrace, so
// an engine call that has finished is answered with its real result. In
// the process, the `stop` request that ends the session is still in
// flight when engine.Run returns and Serve's ctx is cancelled: were its
// ctx cancelled too, Engine.Stop could report "context canceled" for a
// stop that succeeded.
func Serve(ctx context.Context, ln net.Listener, c Controller) error {
	streamCtx, cancelStreams := context.WithCancel(ctx)
	reqCtx, cancelReqs := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelReqs()
	var wg sync.WaitGroup
	defer func() {
		cancelStreams()
		waited := make(chan struct{})
		go func() { wg.Wait(); close(waited) }()
		select {
		case <-waited:
		case <-time.After(ShutdownGrace):
			cancelReqs()
			<-waited
		}
	}()
	go func() {
		<-streamCtx.Done()
		ln.Close()
	}()
	sem := make(chan struct{}, MaxConns)
	var backoff time.Duration
	for {
		conn, err := ln.Accept()
		if err != nil {
			if streamCtx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				backoff = min(max(2*backoff, 5*time.Millisecond), time.Second)
				time.Sleep(backoff)
				continue
			}
			return fmt.Errorf("ipc: accept: %w", err)
		}
		backoff = 0
		select {
		case sem <- struct{}{}:
		default:
			writeResp(conn, proto.EngineResponse{Result: proto.Fail(proto.CodeBusy, "too many connections")})
			conn.Close()
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			defer conn.Close()
			handle(reqCtx, streamCtx, conn, c)
		}()
	}
}

// handle answers one connection. reqCtx is for status/stop/extend and
// outlives Serve's ctx by up to ShutdownGrace; streamCtx, for subscribe,
// ends with Serve's ctx.
func handle(reqCtx, streamCtx context.Context, conn net.Conn, c Controller) {
	_ = conn.SetReadDeadline(time.Now().Add(RequestReadTimeout))
	lr := proto.NewLineReader(conn, proto.MaxEngineRequestBytes)
	var req proto.EngineRequest
	if err := lr.Decode(&req, false); err != nil {
		var de *proto.DecodeError
		switch {
		case errors.Is(err, proto.ErrLineTooLong):
			writeResp(conn, proto.EngineResponse{Result: proto.Fail(proto.CodeTooLarge, "request exceeds %d bytes", proto.MaxEngineRequestBytes)})
		case errors.As(err, &de):
			writeResp(conn, proto.EngineResponse{Result: proto.Fail(proto.CodeBadRequest, "%v", err)})
		}
		return // EOF, timeout, I/O error: nothing to answer
	}
	_ = conn.SetReadDeadline(time.Time{})
	if err := req.Validate(); err != nil {
		writeResp(conn, proto.EngineResponse{Result: errResult(err)})
		return
	}

	switch req.Op {
	case proto.OpEngineStatus:
		snap := c.Snapshot()
		writeResp(conn, proto.EngineResponse{Result: proto.OKResult(), Snapshot: &snap})
	case proto.OpEngineStop:
		if err := c.Stop(reqCtx, req.Sleep); err != nil {
			writeResp(conn, proto.EngineResponse{Result: errResult(err)})
			return
		}
		snap := c.Snapshot()
		writeResp(conn, proto.EngineResponse{Result: proto.OKResult(), Snapshot: &snap})
	case proto.OpEngineExtend:
		snap, err := c.Extend(reqCtx, req.Minutes, req.Forever)
		if err != nil {
			writeResp(conn, proto.EngineResponse{Result: errResult(err)})
			return
		}
		writeResp(conn, proto.EngineResponse{Result: proto.OKResult(), Snapshot: &snap})
	case proto.OpEngineAbortSleep:
		snap, err := c.AbortSleep(reqCtx)
		if err != nil {
			writeResp(conn, proto.EngineResponse{Result: errResult(err)})
			return
		}
		writeResp(conn, proto.EngineResponse{Result: proto.OKResult(), Snapshot: &snap})
	case proto.OpEngineSubscribe:
		subscribe(streamCtx, conn, c)
	}
}

// subscribe streams snapshots until the final one (done=true), the client
// disconnects, a write stalls, or ctx ends.
func subscribe(ctx context.Context, conn net.Conn, c Controller) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		// The client sends nothing more; EOF or an error means it left.
		_, _ = io.Copy(io.Discard, conn)
		cancel()
	}()
	for snap := range c.Subscribe(ctx) {
		resp := proto.EngineResponse{Result: proto.OKResult(), Snapshot: &snap, Done: snap.Session.Ended}
		if err := writeResp(conn, resp); err != nil {
			return // slow or gone: drop the subscriber
		}
		if resp.Done {
			return
		}
	}
}

func writeResp(conn net.Conn, r proto.EngineResponse) error {
	_ = conn.SetWriteDeadline(time.Now().Add(proto.SubscribeWriteTimeout))
	return proto.WriteLine(conn, r)
}

// errResult maps engine errors onto protocol codes.
func errResult(err error) proto.Result {
	var re *proto.RemoteError
	switch {
	case errors.As(err, &re):
		return proto.Result{Code: re.Code, Error: re.Message}
	case errors.Is(err, contracts.ErrNotAbortable):
		return proto.Fail(proto.CodeNotAbortable, "%v", err)
	case errors.Is(err, contracts.ErrNoSleepPending):
		return proto.Fail(proto.CodeNoSleepPending, "%v", err)
	default:
		return proto.Fail(proto.CodeInternal, "%v", err)
	}
}
