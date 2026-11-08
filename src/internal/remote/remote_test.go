package remote

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/takezoh/syntoria-reach/src/internal/core"
)

const token = "node-token-0123456789abcdef0123456789"

// gateway is a fake gateway: it accepts connections with the right token
// and hands each one to the test.
type gateway struct {
	srv      *httptest.Server
	conns    chan *websocket.Conn
	attempts atomic.Int32
	rejected atomic.Int32
}

func newGateway(t *testing.T, rejectFirst int32) *gateway {
	g := &gateway{conns: make(chan *websocket.Conn, 10)}
	g.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := g.attempts.Add(1)
		if r.Header.Get("Authorization") != "Bearer "+token || n <= rejectFirst {
			g.rejected.Add(1)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		g.conns <- conn
		<-r.Context().Done()
	}))
	t.Cleanup(g.srv.Close)
	return g
}

func (g *gateway) url() string { return "ws" + strings.TrimPrefix(g.srv.URL, "http") + "/connect" }

func (g *gateway) next(t *testing.T) *websocket.Conn {
	t.Helper()
	select {
	case c := <-g.conns:
		t.Cleanup(func() { c.CloseNow() })
		return c
	case <-time.After(5 * time.Second):
		t.Fatal("the connector did not connect")
		return nil
	}
}

func run(t *testing.T, c *Connector) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("Run did not return after its context ended")
		}
	})
}

func handler(_ context.Context, method string, params []byte) (any, error) {
	switch method {
	case "echo":
		var p map[string]any
		json.Unmarshal(params, &p)
		return p, nil
	case "missing":
		return nil, core.Errorf(core.CodeNotFound, "no such session")
	}
	return nil, errors.New("disk on fire")
}

func TestRequests(t *testing.T) {
	g := newGateway(t, 0)
	run(t, &Connector{URL: g.url(), Token: token, Handler: handler})
	conn := g.next(t)
	ctx := context.Background()

	for _, m := range []string{
		`{"id": "1", "method": "echo", "params": {"session_id": "as_1"}}`,
		`{"id": "2", "method": "missing", "params": {}}`,
		`{"id": "3", "method": "boom", "params": {}}`,
		`not json`, // ignored
	} {
		if err := conn.Write(ctx, websocket.MessageText, []byte(m)); err != nil {
			t.Fatal(err)
		}
	}
	got := map[string]response{}
	for len(got) < 3 {
		_, data, err := conn.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var r response
		if err := json.Unmarshal(data, &r); err != nil {
			t.Fatalf("response %q: %v", data, err)
		}
		got[r.ID] = r
	}
	if r := got["1"]; r.Error != nil || string(r.Result) != `{"session_id":"as_1"}` {
		t.Errorf("echo: %+v", r)
	}
	if r := got["2"]; r.Result != nil || r.Error == nil || r.Error.Code != core.CodeNotFound || r.Error.Message != "no such session" {
		t.Errorf("classified error: %+v", r)
	}
	// Unclassified errors do not leak their message.
	if r := got["3"]; r.Error == nil || r.Error.Code != core.CodeInternal || r.Error.Message != "internal error" {
		t.Errorf("internal error: %+v", r)
	}
}

func TestReconnects(t *testing.T) {
	// The first two dials are refused; the connector keeps trying.
	g := newGateway(t, 2)
	run(t, &Connector{URL: g.url(), Token: token, Handler: handler, MinBackoff: 10 * time.Millisecond})
	first := g.next(t)
	if n := g.rejected.Load(); n != 2 {
		t.Fatalf("rejected %d dials, want 2", n)
	}
	// The gateway drops the connection; the connector comes back and
	// serves on the new one.
	first.Close(websocket.StatusGoingAway, "restart")
	second := g.next(t)
	ctx := context.Background()
	second.Write(ctx, websocket.MessageText, []byte(`{"id": "a", "method": "echo", "params": {"x": 1}}`))
	_, data, err := second.Read(ctx)
	if err != nil || !strings.Contains(string(data), `"result":{"x":1}`) {
		t.Fatalf("after reconnect: %s %v", data, err)
	}
}

func TestWrongTokenIsRefused(t *testing.T) {
	g := newGateway(t, 0)
	run(t, &Connector{URL: g.url(), Token: "wrong-token-0123456789abcdef0123456", Handler: handler, MinBackoff: 10 * time.Millisecond})
	deadline := time.Now().Add(5 * time.Second)
	for g.rejected.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if g.rejected.Load() < 2 || len(g.conns) != 0 {
		t.Fatalf("rejected=%d accepted=%d", g.rejected.Load(), len(g.conns))
	}
}

func TestDeadConnectionIsReplaced(t *testing.T) {
	g := newGateway(t, 0)
	run(t, &Connector{URL: g.url(), Token: token, Handler: handler, PingInterval: 50 * time.Millisecond, MinBackoff: 10 * time.Millisecond})
	silent := g.next(t)
	// The connector pings; this gateway never answers.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, data, err := silent.Read(ctx); err != nil || string(data) != "ping" {
		t.Fatalf("keepalive: %q %v", data, err)
	}
	// Without a pong (or anything else) the connection is given up.
	g.next(t)
}

func TestAnsweredPingsKeepTheConnection(t *testing.T) {
	g := newGateway(t, 0)
	run(t, &Connector{URL: g.url(), Token: token, Handler: handler, PingInterval: 50 * time.Millisecond})
	conn := g.next(t)
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			break // the deadline: the connection stayed up the whole time
		}
		if string(data) == "ping" {
			conn.Write(ctx, websocket.MessageText, []byte("pong"))
		}
	}
	if n := g.attempts.Load(); n != 1 {
		t.Fatalf("connected %d times, want 1", n)
	}
}
