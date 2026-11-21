package remote

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/takezoh/syntoria-reach/src/internal/agent"
	"github.com/takezoh/syntoria-reach/src/internal/agent/agenttest"
	"github.com/takezoh/syntoria-reach/src/internal/config"
	"github.com/takezoh/syntoria-reach/src/internal/core"
	"github.com/takezoh/syntoria-reach/src/internal/localws"
	"github.com/takezoh/syntoria-reach/src/internal/service"
	"github.com/takezoh/syntoria-reach/src/internal/service/servicetest"
	"github.com/takezoh/syntoria-reach/src/internal/store"
	"github.com/takezoh/syntoria-reach/src/internal/workspace"
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

type safeLog struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *safeLog) Write(p []byte) (int, error) { l.mu.Lock(); defer l.mu.Unlock(); return l.b.Write(p) }
func (l *safeLog) String() string              { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

type relayWorkspace struct{ service.Workspaces }

func (relayWorkspace) Session(_ context.Context, id string) (*core.AgentStatus, error) {
	switch id {
	case "missing":
		return nil, core.ErrNotFound
	case "unavailable":
		return nil, errors.New("secret reply must not appear")
	default:
		return &core.AgentStatus{AgentSession: core.AgentSession{State: core.AgentIdle}, Reply: "secret reply must not appear"}, nil
	}
}

func TestEventsOverWebSocket(t *testing.T) {
	var logs safeLog
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	defer slog.SetDefault(previousLogger)
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { db.Close() }()
	cfg := &config.Config{Clients: []config.Client{{ID: "allow", Allow: []string{core.PermAgentRun}}, {ID: "deny"}}}
	makeService := func() *service.Service {
		return service.New(cfg, service.Deps{Store: db, Workspaces: relayWorkspace{}, NewID: servicetest.NewIDs(), Now: func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }})
	}
	svc := makeService()
	allow, _ := svc.ClientByID("allow")
	deny, _ := svc.ClientByID("deny")
	g := newGateway(t, 0)
	connector := func(s *service.Service) *Connector {
		return &Connector{URL: g.url(), Token: token, MinBackoff: 10 * time.Millisecond, Handler: handler, PendingTurnStops: s.PendingTurnStops, AckTurnStop: s.AckTurnStop, TurnStopChanges: s.TurnStopChanges(), AuthorizeEvents: func(ctx context.Context, id string) string {
			if id == "denied" {
				return s.AuthorizeEvents(ctx, deny, id)
			}
			return s.AuthorizeEvents(ctx, allow, id)
		}}
	}
	commit := func(op string) {
		t.Helper()
		tr := core.Turn{ClientID: "c", OperationID: op, SessionID: "session"}
		if err := db.BeginTurn(tr); err != nil {
			t.Fatal(err)
		}
		if err := db.ArmTurn(tr); err != nil {
			t.Fatal(err)
		}
		if err := svc.ObserveTurns(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	commit("one")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { connector(svc).Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	conn := g.next(t)
	read := func(c *websocket.Conn) []byte {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_, b, err := c.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	write := func(c *websocket.Conn, b string) {
		t.Helper()
		if err := c.Write(context.Background(), websocket.MessageText, []byte(b)); err != nil {
			t.Fatal(err)
		}
	}
	first := read(conn)
	var one struct {
		Type string `json:"type"`
		core.TurnStop
	}
	if err := json.Unmarshal(first, &one); err != nil || one.Type != "turn_stop" || one.EventID == "" || one.SessionID != "session" || one.State != core.AgentIdle {
		t.Fatalf("first: %s %v", first, err)
	}
	for _, tc := range []struct{ id, want string }{{"allowed", "allowed"}, {"denied", "denied"}, {"missing", "denied"}, {"unavailable", "unavailable"}} {
		write(conn, fmt.Sprintf(`{"id":"%s","method":"events.authorize","params":{"session_id":"%s","subscription_key":"key","expires_at":"later"}}`, tc.id, tc.id))
		b := read(conn)
		var response struct {
			ID     string `json:"id"`
			Result struct {
				Decision string `json:"decision"`
			} `json:"result"`
		}
		if err := json.Unmarshal(b, &response); err != nil || response.ID != tc.id || response.Result.Decision != tc.want || strings.Contains(string(b), "secret reply") {
			t.Fatalf("authorize %s: %s %v", tc.id, b, err)
		}
	}
	if strings.Contains(logs.String(), "secret reply") {
		t.Fatalf("reply leaked in logs: %s", logs.String())
	}
	write(conn, `{"id":"bad","method":"events.authorize","params":{"session_id":"x"}}`)
	var bad response
	b := read(conn)
	if json.Unmarshal(b, &bad) != nil || bad.Error == nil || bad.Error.Code != core.CodeInvalidRequest {
		t.Fatalf("bad: %s", b)
	}
	commit("two")
	second := read(conn)
	var two struct {
		Type string `json:"type"`
		core.TurnStop
	}
	if json.Unmarshal(second, &two) != nil || two.EventID == one.EventID || two.Type != "turn_stop" {
		t.Fatalf("second: %s", second)
	}
	write(conn, `{"type":"turn_stop_ack","eventId":"unknown"}`)
	write(conn, fmt.Sprintf(`{"type":"turn_stop_ack","eventId":%q}`, one.EventID))
	write(conn, fmt.Sprintf(`{"type":"turn_stop_ack","eventId":%q}`, one.EventID))
	// A request after the acks establishes that the reader processed them.
	write(conn, `{"id":"barrier","method":"echo","params":{}}`)
	read(conn)
	pending, err := db.PendingTurnStops()
	if err != nil || len(pending) != 1 || pending[0].EventID != two.EventID {
		t.Fatalf("pending: %+v %v", pending, err)
	}
	conn.CloseNow()
	conn = g.next(t)
	replay := read(conn)
	var replayed struct{ core.TurnStop }
	if json.Unmarshal(replay, &replayed) != nil || replayed.EventID != two.EventID {
		t.Fatalf("replay: %s", replay)
	}
	cancel()
	<-done
	// Restart the daemon and reopen its durable database.
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	svc = makeService()
	restartCtx, stopRestarted := context.WithCancel(context.Background())
	restarted := make(chan struct{})
	go func() { connector(svc).Run(restartCtx); close(restarted) }()
	defer func() { stopRestarted(); <-restarted }()
	conn = g.next(t)
	replay = read(conn)
	if json.Unmarshal(replay, &replayed) != nil || replayed.EventID != two.EventID {
		t.Fatalf("restart: %s", replay)
	}
	write(conn, fmt.Sprintf(`{"type":"turn_stop_ack","eventId":%q}`, two.EventID))
	write(conn, `{"id":"barrier","method":"echo","params":{}}`)
	read(conn)
	pending, err = db.PendingTurnStops()
	if err != nil || len(pending) != 0 {
		t.Fatalf("after ack: %+v %v", pending, err)
	}
}

func TestRelayLocalAgentFake(t *testing.T) { agenttest.MaybeRun() }

func TestRelayLocalCompletion(t *testing.T) {
	root := t.TempDir()
	db, err := store.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	cmd, env := agenttest.Agent(t, "codex", "TestRelayLocalAgentFake")
	cfg := &config.Config{Workspaces: filepath.Join(root, "workspaces"), Agents: []config.Agent{{ID: "a", Protocol: "codex", Command: cmd, Env: env}}, Clients: []config.Client{{ID: "c", Token: "local-token", Allow: []string{core.PermWorkspaceCreate, core.PermAgentRun}}}}
	f := servicetest.New()
	runtime := agent.New(filepath.Join(root, "logs"))
	defer runtime.Close()
	deps := f.Deps(localws.New(cfg.Workspaces, db, workspace.Folders{}, runtime, f.Deps(nil).NewID))
	deps.Store = db
	deps.Now = time.Now
	svc := service.New(cfg, deps)
	defer svc.Close()
	client, _ := svc.ClientByID("c")
	ctx := context.Background()
	op, err := svc.CreateWorkspace(ctx, client, service.CreateWorkspaceRequest{OperationID: "workspace", Name: "test", Note: "test"})
	if err != nil || op.State != core.StateSucceeded {
		t.Fatalf("workspace: %v %v", op, err)
	}
	var w core.Workspace
	if err := json.Unmarshal(op.Result, &w); err != nil {
		t.Fatal(err)
	}
	op, err = svc.StartAgentSession(ctx, client, service.StartAgentSessionRequest{OperationID: "start", Workspace: w.ID, Agent: "a"})
	if err != nil || op.State != core.StateSucceeded {
		t.Fatalf("start: %v %v", op, err)
	}
	var session core.AgentSession
	if err := json.Unmarshal(op.Result, &session); err != nil {
		t.Fatal(err)
	}
	g := newGateway(t, 0)
	scanned := make(chan struct{}, 1)
	c := &Connector{URL: g.url(), Token: token, Handler: handler, PendingTurnStops: func() ([]core.TurnStop, error) {
		stops, err := svc.PendingTurnStops()
		select {
		case scanned <- struct{}{}:
		default:
		}
		return stops, err
	}, AckTurnStop: svc.AckTurnStop, TurnStopChanges: svc.TurnStopChanges()}
	relayCtx, stopRelay := context.WithCancel(context.Background())
	relayDone := make(chan struct{})
	go func() { c.Run(relayCtx); close(relayDone) }()
	defer func() { stopRelay(); <-relayDone }()
	conn := g.next(t)
	select {
	case <-scanned:
	case <-time.After(3 * time.Second):
		t.Fatal("initial scan missing")
	}
	op, err = svc.SendAgentPrompt(ctx, client, service.SendAgentPromptRequest{OperationID: "prompt", SessionID: session.ID, Text: "hello"})
	if err != nil || op.State != core.StateSucceeded {
		t.Fatalf("prompt: %v %v", op, err)
	}
	readCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	_, data, err := conn.Read(readCtx)
	if err != nil {
		pending, e := db.PendingTurnStops()
		t.Fatalf("no frame on connected socket: %v, pending=%+v (%v)", err, pending, e)
	}
	var frame struct {
		Type string `json:"type"`
		core.TurnStop
	}
	if err := json.Unmarshal(data, &frame); err != nil {
		t.Fatal(err)
	}
	pending, err := db.PendingTurnStops()
	if err != nil || len(pending) != 1 || frame.Type != "turn_stop" || frame.EventID != pending[0].EventID || frame.SessionID != session.ID || frame.State != core.AgentIdle {
		t.Fatalf("frame=%s pending=%+v err=%v", data, pending, err)
	}
	if g.attempts.Load() != 1 {
		t.Fatalf("unexpected reconnect: %d", g.attempts.Load())
	}
}

func TestRelayPendingCommitOrder(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	for _, id := range []string{"first", "second", "third"} {
		tr := core.Turn{ClientID: "c", OperationID: id, SessionID: "session"}
		if err := db.BeginTurn(tr); err != nil {
			t.Fatal(err)
		}
		if err := db.ArmTurn(tr); err != nil {
			t.Fatal(err)
		}
		if err := db.CloseTurn(tr, core.TurnStop{EventID: id, SessionID: tr.SessionID, State: core.AgentIdle, ObservedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	g := newGateway(t, 0)
	run(t, &Connector{URL: g.url(), Token: token, Handler: handler, PendingTurnStops: db.PendingTurnStops, AckTurnStop: db.DeleteTurnStop})
	conn := g.next(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for _, want := range []string{"first", "second", "third"} {
		_, data, err := conn.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var frame struct {
			Type    string `json:"type"`
			EventID string `json:"eventId"`
		}
		if json.Unmarshal(data, &frame) != nil || frame.Type != "turn_stop" || frame.EventID != want {
			t.Fatalf("expected %s, got %s", want, data)
		}
	}
}
