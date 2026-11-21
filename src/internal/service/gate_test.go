//go:build gate

// Real-environment checks of the turn-stop gates in
// docs/changes/change-20261119-mcp-events/architecture-plan.json
// (g-session-loss and g-start-reconciliation). They drive a real Orca server
// and a real Claude Code through the same service path as the daemon, with a
// throwaway SQLite store, and remove the workspace they create.
//
//	REACH_GATE_ORCA=/path/to/orca-cli \
//	REACH_GATE_WORKSPACES=/folder/trusted/by/claude \
//	REACH_GATE_CLAUDE=/path/to/claude \
//	go test -tags gate -run TestGate -v ./internal/service/
package service_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/takezoh/syntoria-reach/src/internal/agent"
	"github.com/takezoh/syntoria-reach/src/internal/config"
	"github.com/takezoh/syntoria-reach/src/internal/core"
	"github.com/takezoh/syntoria-reach/src/internal/localws"
	"github.com/takezoh/syntoria-reach/src/internal/orca"
	"github.com/takezoh/syntoria-reach/src/internal/orcaws"
	"github.com/takezoh/syntoria-reach/src/internal/service"
	"github.com/takezoh/syntoria-reach/src/internal/service/servicetest"
	"github.com/takezoh/syntoria-reach/src/internal/store"
	"github.com/takezoh/syntoria-reach/src/internal/workspace"
)

type gateEnv struct {
	t     *testing.T
	ctx   context.Context
	cli   *orca.CLI
	ws    *orcaws.Workspaces
	db    *store.Store
	path  string
	cfg   *config.Config
	svc   *service.Service
	c     *service.Client
	space core.Workspace
}

func newGate(t *testing.T) *gateEnv {
	orcaCmd, root, claude := os.Getenv("REACH_GATE_ORCA"), os.Getenv("REACH_GATE_WORKSPACES"), os.Getenv("REACH_GATE_CLAUDE")
	if orcaCmd == "" || root == "" || claude == "" {
		t.Skip("set REACH_GATE_ORCA, REACH_GATE_WORKSPACES and REACH_GATE_CLAUDE")
	}
	g := &gateEnv{t: t, ctx: context.Background(), cli: &orca.CLI{Command: []string{orcaCmd}}}
	g.ws = orcaws.New(g.cli, workspace.Folders{}, root)
	g.path = filepath.Join(t.TempDir(), "state.db")
	// A small model keeps the check cheap; the turn's shape is what matters.
	g.cfg = &config.Config{Workspaces: root, Agents: []config.Agent{{ID: "a", Command: []string{claude, "--model", "claude-haiku-5-5"}}}, Clients: []config.Client{{ID: "c", Token: tokenA, Allow: []string{core.PermWorkspaceCreate, core.PermAgentRun}}}}
	g.open(nil)
	op, err := g.svc.CreateWorkspace(g.ctx, g.c, service.CreateWorkspaceRequest{OperationID: "w", Name: "reach-gate-" + time.Now().Format("20060102-150405"), Note: "turn-stop gate check; removed by the test"})
	if err != nil || op.State != core.StateSucceeded {
		t.Fatalf("workspace: %v %v", op, err)
	}
	g.space = result[core.Workspace](t, op)
	t.Cleanup(func() {
		if err := g.ws.Remove(context.Background(), &g.space); err != nil {
			t.Errorf("remove workspace %s: %v", g.space.ID, err)
		}
	})
	return g
}

// open (re)starts the service on the same SQLite file, as a daemon restart
// does; wrap, when set, replaces the store the service uses.
func (g *gateEnv) open(wrap func(service.Store) service.Store) {
	g.t.Helper()
	if g.svc != nil {
		g.svc.Close()
		g.db.Close()
	}
	db, err := store.Open(g.path)
	if err != nil {
		g.t.Fatal(err)
	}
	g.db = db
	deps := servicetest.New().Deps(g.ws)
	deps.Store, deps.Now, deps.ObserveTurns = db, time.Now, true
	if wrap != nil {
		deps.Store = wrap(db)
	}
	g.svc = service.New(g.cfg, deps)
	g.c, _ = g.svc.Authenticate(tokenA)
	g.t.Cleanup(func() { g.svc.Close(); g.db.Close() })
}

func (g *gateEnv) session(opID string) string {
	g.t.Helper()
	op, err := g.svc.StartAgentSession(g.ctx, g.c, service.StartAgentSessionRequest{OperationID: opID, Workspace: g.space.ID, Agent: "a"})
	if err != nil || op.State != core.StateSucceeded {
		g.t.Fatalf("start: %v %v", op, err)
	}
	id := result[core.AgentSession](g.t, op).ID
	g.t.Cleanup(func() { g.cli.CloseTerminal(context.Background(), id) })
	return id
}

func (g *gateEnv) prompt(opID, id, text string) (*core.Operation, error) {
	return g.svc.SendAgentPrompt(g.ctx, g.c, service.SendAgentPromptRequest{OperationID: opID, SessionID: id, Text: text})
}

// until observes armed turns every second until done reports true.
func (g *gateEnv) until(what string, limit time.Duration, done func() bool) {
	g.t.Helper()
	deadline := time.Now().Add(limit)
	for !done() {
		if time.Now().After(deadline) {
			g.t.Fatalf("timed out waiting for %s", what)
		}
		if err := g.svc.ObserveTurns(g.ctx); err != nil {
			g.t.Logf("observe: %v", err)
		}
		time.Sleep(time.Second)
	}
}

func (g *gateEnv) counts() (armed, stops int) {
	g.t.Helper()
	a, err := g.db.ArmedTurns()
	if err != nil {
		g.t.Fatal(err)
	}
	s, err := g.db.PendingTurnStops()
	if err != nil {
		g.t.Fatal(err)
	}
	return len(a), len(s)
}

// A normal turn on the real Orca ends in exactly one idle stop.
func TestGateOrcaTurnStop(t *testing.T) {
	g := newGate(t)
	id := g.session("s")
	start := time.Now()
	if op, err := g.prompt("p", id, "Reply with the single word OK."); err != nil || op.State != core.StateSucceeded {
		t.Fatalf("prompt: %v %v", op, err)
	}
	g.until("the stop", 3*time.Minute, func() bool { _, n := g.counts(); return n == 1 })
	stops, _ := g.db.PendingTurnStops()
	if stops[0].State != core.AgentIdle || stops[0].SessionID != id {
		t.Fatalf("stop: %+v", stops[0])
	}
	if armed, _ := g.counts(); armed != 0 {
		t.Fatal("still armed")
	}
	t.Logf("stop %s after %s", stops[0].State, time.Since(start).Round(time.Second))
}

// g-session-loss on Orca: a terminal closed outside Reach during an armed turn
// is lost, not exited: the turn is disarmed once and no stop is recorded.
func TestGateOrcaTerminalClosedDuringTurn(t *testing.T) {
	g := newGate(t)
	id := g.session("s")
	if op, err := g.prompt("p", id, "Write a story of about 3000 words about a lighthouse keeper. Do not use any tools."); err != nil || op.State != core.StateSucceeded {
		t.Fatalf("prompt: %v %v", op, err)
	}
	st, err := g.ws.Session(g.ctx, id)
	if err != nil || st.State != core.AgentWorking {
		t.Fatalf("turn is not running, cannot close it mid-turn: state=%s err=%v", stateOf(st), err)
	}
	if err := g.cli.CloseTerminal(g.ctx, id); err != nil {
		t.Fatal(err)
	}
	_, err = g.ws.Session(g.ctx, id)
	t.Logf("session after close: %v", err)
	if !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("closed terminal is not not_found: %v", err)
	}
	if err := g.svc.ObserveTurns(g.ctx); err != nil {
		t.Fatal(err)
	}
	if armed, stops := g.counts(); armed != 0 || stops != 0 {
		t.Fatalf("armed=%d stops=%d, want 0 0", armed, stops)
	}
}

// A real Claude Code run by the daemon over stream-json records one idle stop
// from the protocol, and a daemon restart loses the session without a stop.
func TestGateLocalClaude(t *testing.T) {
	root, claude := os.Getenv("REACH_GATE_WORKSPACES"), os.Getenv("REACH_GATE_CLAUDE")
	if root == "" || claude == "" {
		t.Skip("set REACH_GATE_WORKSPACES and REACH_GATE_CLAUDE")
	}
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { db.Close() }()
	cfg := &config.Config{Workspaces: root, Agents: []config.Agent{{ID: "a", Protocol: core.ProtocolClaude, Command: []string{claude, "--model", "claude-haiku-5-5"}}}, Clients: []config.Client{{ID: "c", Token: tokenA, Allow: []string{core.PermWorkspaceCreate, core.PermAgentRun}}}}
	f := servicetest.New()
	runtime := agent.New(filepath.Join(t.TempDir(), "logs"))
	ws := localws.New(root, db, workspace.Folders{}, runtime, f.Deps(nil).NewID)
	deps := f.Deps(ws)
	deps.Store, deps.Now = db, time.Now
	svc := service.New(cfg, deps)
	c, _ := svc.Authenticate(tokenA)
	op, err := svc.CreateWorkspace(ctx, c, service.CreateWorkspaceRequest{OperationID: "w", Name: "reach-gate-local-" + time.Now().Format("20060102-150405"), Note: "turn-stop gate check; removed by the test"})
	if err != nil || op.State != core.StateSucceeded {
		t.Fatalf("workspace: %v %v", op, err)
	}
	space := result[core.Workspace](t, op)
	t.Cleanup(func() { os.RemoveAll(space.Path) })
	op, err = svc.StartAgentSession(ctx, c, service.StartAgentSessionRequest{OperationID: "s", Workspace: space.ID, Agent: "a"})
	if err != nil || op.State != core.StateSucceeded {
		t.Fatalf("start: %v %v", op, err)
	}
	id := result[core.AgentSession](t, op).ID
	start := time.Now()
	if op, err = svc.SendAgentPrompt(ctx, c, service.SendAgentPromptRequest{OperationID: "p", SessionID: id, Text: "Reply with the single word OK."}); err != nil || op.State != core.StateSucceeded {
		t.Fatalf("prompt: %v %v", op, err)
	}
	deadline := time.Now().Add(3 * time.Minute)
	for {
		stops, err := db.PendingTurnStops()
		if err != nil {
			t.Fatal(err)
		}
		if len(stops) == 1 {
			if stops[0].State != core.AgentIdle {
				t.Fatalf("stop: %+v", stops[0])
			}
			t.Logf("stop %s after %s", stops[0].State, time.Since(start).Round(time.Second))
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no stop")
		}
		time.Sleep(time.Second)
	}
	long := "Write a story of about 3000 words. Do not use any tools."
	// A graceful daemon stop ends its agents itself and witnesses the exit:
	// the armed turn stops as exited.
	if op, err = svc.SendAgentPrompt(ctx, c, service.SendAgentPromptRequest{OperationID: "p2", SessionID: id, Text: long}); err != nil || op.State != core.StateSucceeded {
		t.Fatalf("prompt 2: %v %v", op, err)
	}
	svc.Close()
	runtime.Close()
	stops, _ := db.PendingTurnStops()
	if len(stops) != 2 || stops[1].State != core.AgentExited {
		t.Fatalf("graceful stop: %+v", stops)
	}
	db.Close()

	// A daemon crash witnesses nothing: the next daemon finds the armed turn,
	// its session is gone, and the turn is lost without a stop. The crashed
	// runtime is abandoned rather than closed, as a killed process would be.
	if db, err = store.Open(path); err != nil {
		t.Fatal(err)
	}
	crashed := agent.New(filepath.Join(t.TempDir(), "logs2"))
	deps.Store, deps.Workspaces = db, localws.New(root, db, workspace.Folders{}, crashed, f.Deps(nil).NewID)
	svc = service.New(cfg, deps)
	op, err = svc.StartAgentSession(ctx, c, service.StartAgentSessionRequest{OperationID: "s2", Workspace: space.ID, Agent: "a"})
	if err != nil || op.State != core.StateSucceeded {
		t.Fatalf("start 2: %v %v", op, err)
	}
	id2 := result[core.AgentSession](t, op).ID
	if op, err = svc.SendAgentPrompt(ctx, c, service.SendAgentPromptRequest{OperationID: "p3", SessionID: id2, Text: long}); err != nil || op.State != core.StateSucceeded {
		t.Fatalf("prompt 3: %v %v", op, err)
	}
	db.Close() // the crash: nothing more is written by this daemon
	defer crashed.Close()

	if db, err = store.Open(path); err != nil {
		t.Fatal(err)
	}
	defer func() { db.Close() }()
	fresh := agent.New(filepath.Join(t.TempDir(), "logs3"))
	defer fresh.Close()
	deps.Store, deps.Workspaces = db, localws.New(root, db, workspace.Folders{}, fresh, f.Deps(nil).NewID)
	svc = service.New(cfg, deps)
	defer svc.Close()
	if err := svc.ObserveTurns(ctx); err != nil {
		t.Fatal(err)
	}
	armed, _ := db.ArmedTurns()
	stops, _ = db.PendingTurnStops()
	t.Logf("after crash restart: armed=%d stops=%d", len(armed), len(stops))
	if len(armed) != 0 || len(stops) != 2 {
		t.Fatalf("crash restart: armed=%v stops=%v", armed, stops)
	}
}

func stateOf(st *core.AgentStatus) string {
	if st == nil {
		return "<none>"
	}
	return st.State
}

type failArm struct{ service.Store }

func (failArm) ArmTurn(core.Turn) error { return errors.New("injected crash before the arm commit") }

// g-start-reconciliation: the prompt reached the agent but the arm was never
// committed (the daemon died in between). Does anything after a restart
// recover the turn from the pending intent and the operation receipt?
func TestGateOrcaStartWithoutArm(t *testing.T) {
	g := newGate(t)
	id := g.session("s")
	g.open(func(s service.Store) service.Store { return failArm{s} })
	_, err := g.prompt("p", id, "Reply with the single word OK.")
	var e *core.Error
	if !errors.As(err, &e) || e.Code != core.CodeResultUnknown {
		t.Fatalf("prompt with a lost arm: %v", err)
	}
	g.open(nil) // restart on the same database
	op, err := g.svc.GetOperation(g.c, service.GetOperationRequest{OperationID: "p"})
	t.Logf("operation after restart: %+v %v", op, err)
	time.Sleep(20 * time.Second) // let the delivered turn finish
	if err := g.svc.ObserveTurns(g.ctx); err != nil {
		t.Fatal(err)
	}
	st, err := g.ws.Session(g.ctx, id)
	armed, stops := g.counts()
	t.Logf("after restart: session=%+v err=%v armed=%d stops=%d", st, err, armed, stops)
}
