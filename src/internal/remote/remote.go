// Package remote keeps the daemon connected to the Reach gateway on
// Cloudflare. The daemon dials out, so it needs no inbound port; it serves
// the requests that arrive on the WebSocket with a handler, and reconnects
// with backoff when the connection drops.
//
// The connection only carries requests and responses. Losing it never stops
// work a request started; the gateway reports the outcome of a request in
// flight as unknown, and the caller reconciles it by operation ID.
//
// Wire format: text messages of JSON. The gateway sends
// {"id", "method", "params"}; the daemon answers {"id", "result"} or
// {"id", "error": {"code", "message"}}. The ID correlates the two on one
// connection and is unrelated to operation IDs. The daemon also sends
// "ping", which the gateway answers with "pong".
package remote

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/coder/websocket"

	"github.com/takezoh/syntoria-reach/src/internal/core"
)

const (
	maxMessageSize = 16 << 20
	dialTimeout    = 30 * time.Second
	writeTimeout   = 10 * time.Second
)

// Handler serves one request: a Reach method and its JSON params.
type Handler func(ctx context.Context, method string, params []byte) (any, error)

// Connector keeps one connection to the gateway open while Run runs.
type Connector struct {
	URL     string // ws:// or wss:// connect endpoint of the gateway
	Token   string // node token, sent as a bearer token
	Handler Handler

	// The connector pings every PingInterval and treats the connection as
	// dead when nothing arrives for three intervals. Reconnection waits
	// MinBackoff, doubling up to MaxBackoff while dialing fails. Zero values
	// mean 30s, 1s and 1m.
	PingInterval time.Duration
	MinBackoff   time.Duration
	MaxBackoff   time.Duration
}

type request struct {
	ID     string          `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

type response struct {
	ID     string          `json:"id"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *core.Error     `json:"error,omitempty"`
}

// Run connects and serves until ctx is done.
func (c *Connector) Run(ctx context.Context) {
	minBackoff := or(c.MinBackoff, time.Second)
	maxBackoff := or(c.MaxBackoff, time.Minute)
	backoff := minBackoff
	for {
		connected, err := c.session(ctx)
		if ctx.Err() != nil {
			return
		}
		if connected {
			backoff = minBackoff
		}
		slog.Warn("gateway connection ended", "err", err, "retry_in", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if !connected {
			backoff = min(2*backoff, maxBackoff)
		}
	}
}

// session dials once and serves the connection until it ends. connected
// reports whether the dial succeeded.
func (c *Connector) session(ctx context.Context) (connected bool, err error) {
	dialCtx, cancel := context.WithTimeout(ctx, dialTimeout)
	conn, _, err := websocket.Dial(dialCtx, c.URL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer " + c.Token}},
	})
	cancel()
	if err != nil {
		return false, err
	}
	defer conn.CloseNow()
	conn.SetReadLimit(maxMessageSize)
	slog.Info("connected to gateway", "url", c.URL)

	interval := or(c.PingInterval, 30*time.Second)
	connCtx, stop := context.WithCancel(ctx)
	defer stop()
	go keepalive(connCtx, conn, interval)
	for {
		readCtx, cancelRead := context.WithTimeout(connCtx, 3*interval)
		typ, data, err := conn.Read(readCtx)
		cancelRead()
		if err != nil {
			if ctx.Err() != nil {
				conn.Close(websocket.StatusGoingAway, "daemon stopping")
			}
			return true, err
		}
		if typ != websocket.MessageText || string(data) == "pong" {
			continue
		}
		var req request
		if err := json.Unmarshal(data, &req); err != nil || req.ID == "" {
			slog.Warn("ignoring a malformed message from the gateway")
			continue
		}
		// The request runs under the daemon's context, not the
		// connection's: a disconnect does not cancel accepted work.
		go c.serve(ctx, conn, req)
	}
}

func (c *Connector) serve(ctx context.Context, conn *websocket.Conn, req request) {
	out := response{ID: req.ID}
	result, err := c.Handler(ctx, req.Method, req.Params)
	if err == nil {
		out.Result, err = json.Marshal(result)
	}
	if err != nil {
		var e *core.Error
		if !errors.As(err, &e) {
			e = &core.Error{Code: core.CodeInternal, Message: "internal error"}
		}
		out.Result, out.Error = nil, e
	}
	b, err := json.Marshal(out)
	if err != nil {
		return
	}
	wctx, cancel := context.WithTimeout(context.Background(), writeTimeout)
	defer cancel()
	// A failed write means the connection is gone; the gateway reports
	// the request's outcome as unknown.
	conn.Write(wctx, websocket.MessageText, b)
}

func keepalive(ctx context.Context, conn *websocket.Conn, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := conn.Write(ctx, websocket.MessageText, []byte("ping")); err != nil {
				return
			}
		}
	}
}

func or(d, def time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return def
}
