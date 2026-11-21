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
// events.authorize is an ordinary request with session_id, subscription_key,
// expires_at; its result is {"decision":"allowed|denied|unavailable"}.
// Unsolicited {"type":"turn_stop", "eventId", "session_id", "state",
// "observed_at"} is acknowledged by {"type":"turn_stop_ack", "eventId"}.
// These unsolicited frames have no id; node identity is the connection.
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
	URL              string // ws:// or wss:// connect endpoint of the gateway
	Token            string // node token, sent as a bearer token
	Handler          Handler
	AuthorizeEvents  func(context.Context, string) string
	PendingTurnStops func() ([]core.TurnStop, error)
	AckTurnStop      func(string) error
	TurnStopChanges  <-chan struct{}

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
	if c.PendingTurnStops != nil {
		go c.relay(connCtx, conn)
	}
	unknownTypes := map[string]bool{}
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
		var frame struct {
			Type    string `json:"type"`
			EventID string `json:"eventId"`
		}
		if json.Unmarshal(data, &frame) == nil && frame.Type != "" {
			if frame.Type == "turn_stop_ack" && frame.EventID != "" && c.AckTurnStop != nil {
				if err := c.AckTurnStop(frame.EventID); err != nil {
					return true, err
				}
			} else if frame.Type != "turn_stop_ack" && !unknownTypes[frame.Type] {
				unknownTypes[frame.Type] = true
				slog.Warn("ignoring unknown gateway frame type", "type", frame.Type)
			}
			continue
		}
		var req request
		if err := json.Unmarshal(data, &req); err != nil || req.ID == "" || req.Method == "" {
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
	var result any
	var err error
	if req.Method == "events.authorize" && c.AuthorizeEvents != nil {
		var p struct {
			SessionID       string `json:"session_id"`
			SubscriptionKey string `json:"subscription_key"`
			ExpiresAt       string `json:"expires_at"`
		}
		if json.Unmarshal(req.Params, &p) != nil || p.SessionID == "" || p.SubscriptionKey == "" || p.ExpiresAt == "" {
			err = core.Errorf(core.CodeInvalidRequest, "invalid events.authorize params")
		} else {
			result = struct {
				Decision string `json:"decision"`
			}{c.AuthorizeEvents(ctx, p.SessionID)}
		}
	} else {
		result, err = c.Handler(ctx, req.Method, req.Params)
	}
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

// relay scans on connection and after committed changes. A failed scan or send
// ends the connection so the next connection retries the durable outbox.
func (c *Connector) relay(ctx context.Context, conn *websocket.Conn) {
	sent := map[string]bool{}
	for {
		stops, err := c.PendingTurnStops()
		if err != nil {
			conn.CloseNow()
			return
		}
		pending := make(map[string]bool, len(stops))
		for _, stop := range stops {
			pending[stop.EventID] = true
		}
		for id := range sent {
			if !pending[id] {
				delete(sent, id)
			}
		}
		for _, stop := range stops {
			if sent[stop.EventID] {
				continue
			}
			frame := struct {
				Type string `json:"type"`
				core.TurnStop
			}{"turn_stop", stop}
			frame.ObservedAt = frame.ObservedAt.UTC()
			b, err := json.Marshal(frame)
			if err != nil {
				conn.CloseNow()
				return
			}
			wctx, cancel := context.WithTimeout(ctx, writeTimeout)
			err = conn.Write(wctx, websocket.MessageText, b)
			cancel()
			if err != nil {
				conn.CloseNow()
				return
			}
			sent[stop.EventID] = true
		}
		select {
		case <-ctx.Done():
			return
		case <-c.TurnStopChanges:
		}
	}
}
