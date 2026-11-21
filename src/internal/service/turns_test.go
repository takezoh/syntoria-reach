package service_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/takezoh/syntoria-reach/src/internal/config"
	"github.com/takezoh/syntoria-reach/src/internal/core"
	"github.com/takezoh/syntoria-reach/src/internal/orca"
	"github.com/takezoh/syntoria-reach/src/internal/orca/orcatest"
	"github.com/takezoh/syntoria-reach/src/internal/orcaws"
	"github.com/takezoh/syntoria-reach/src/internal/service"
	"github.com/takezoh/syntoria-reach/src/internal/service/servicetest"
	"github.com/takezoh/syntoria-reach/src/internal/store"
	"github.com/takezoh/syntoria-reach/src/internal/workspace"
)

type controlledOrca struct {
	*servicetest.Orca
	busyAfterSend bool
	readErr       error
}

func (o *controlledOrca) SendPrompt(ctx context.Context, id, text string) ([]string, error) {
	stages, err := o.Orca.SendPrompt(ctx, id, text)
	if err == nil && o.busyAfterSend {
		o.Busy(id, true)
	}
	return stages, err
}
func (o *controlledOrca) ShowTerminal(ctx context.Context, id string) (*core.TerminalStatus, error) {
	if o.readErr != nil {
		return nil, o.readErr
	}
	return o.Orca.ShowTerminal(ctx, id)
}
func TestTurnObservationThroughPrompt(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { db.Close() }()
	f := servicetest.New()
	o := &controlledOrca{Orca: f.Orca}
	cfg := &config.Config{Workspaces: "/w", Agents: []config.Agent{{ID: "a", Command: []string{"agent"}}}, Clients: []config.Client{{ID: "c", Token: tokenA, Allow: []string{core.PermWorkspaceCreate, core.PermAgentRun}}}}
	deps := f.Deps(orcaws.New(o, f.Folders, "/w"))
	deps.Store = db
	deps.Now = func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }
	deps.ObserveTurns = true
	svc := service.New(cfg, deps)
	defer svc.Close()
	c, _ := svc.Authenticate(tokenA)
	op, err := svc.CreateWorkspace(ctx, c, service.CreateWorkspaceRequest{OperationID: "workspace", Name: "test", Note: "test"})
	if err != nil || op.State != core.StateSucceeded {
		t.Fatalf("workspace %v %v", op, err)
	}
	ws := result[core.Workspace](t, op)
	op, err = svc.StartAgentSession(ctx, c, service.StartAgentSessionRequest{OperationID: "start", Workspace: ws.ID, Agent: "a"})
	if err != nil || op.State != core.StateSucceeded {
		t.Fatalf("start %v %v", op, err)
	}
	id := result[core.AgentSession](t, op).ID
	stops := func() []core.TurnStop {
		t.Helper()
		v, e := db.PendingTurnStops()
		if e != nil {
			t.Fatal(e)
		}
		return v
	}
	armed := func() []core.Turn {
		t.Helper()
		v, e := db.ArmedTurns()
		if e != nil {
			t.Fatal(e)
		}
		return v
	}
	if err := svc.ObserveTurns(ctx); err != nil || len(stops()) != 0 {
		t.Fatalf("idle before prompt: %v %v", stops(), err)
	}
	prompt := func(idOp string) *core.Operation {
		t.Helper()
		v, e := svc.SendAgentPrompt(ctx, c, service.SendAgentPromptRequest{OperationID: idOp, SessionID: id, Text: "hello"})
		if e != nil {
			t.Fatal(e)
		}
		return v
	}
	if op := prompt("one"); op.State != core.StateSucceeded {
		t.Fatal(op)
	}
	if got := stops(); len(got) != 1 || got[0].State != core.AgentIdle || got[0].SessionID != id || got[0].EventID == "" {
		t.Fatalf("immediate: %+v", got)
	}
	first := stops()[0]
	prompt("one")
	if len(stops()) != 1 || len(armed()) != 0 {
		t.Fatal("replay rearmed")
	}
	o.SilentAgent = true
	if op := prompt("unconfirmed"); op.State != core.StateFailed {
		t.Fatal(op)
	}
	o.SilentAgent = false
	if len(armed()) != 0 || len(stops()) != 1 {
		t.Fatal("unconfirmed prompt armed")
	}
	o.busyAfterSend = true
	if op := prompt("two"); op.State != core.StateSucceeded {
		t.Fatal(op)
	}
	if len(armed()) != 1 || len(stops()) != 1 {
		t.Fatalf("working: %v %v", armed(), stops())
	}
	o.readErr = errors.New("unavailable")
	if svc.ObserveTurns(ctx) == nil || len(armed()) != 1 || len(stops()) != 1 {
		t.Fatal("read failure lost turn")
	}
	o.readErr = nil
	svc.Close()
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	deps.Store = db
	svc = service.New(cfg, deps)
	defer svc.Close()
	if len(armed()) != 1 || len(stops()) != 1 || stops()[0] != first {
		t.Fatal("restart lost records")
	}
	o.AskUser(id, "permission")
	if err := svc.ObserveTurns(ctx); err != nil {
		t.Fatal(err)
	}
	if got := stops(); len(got) != 2 || got[1].State != core.AgentWaitingOnHuman || got[1].EventID == first.EventID || len(armed()) != 0 {
		t.Fatalf("waiting: %v", got)
	}
	if err := svc.ObserveTurns(ctx); err != nil || len(stops()) != 2 {
		t.Fatal("duplicate observation")
	}
	if err := db.DeleteTurnStop(first.EventID); err != nil || len(stops()) != 1 {
		t.Fatal("delete")
	}
	o.AskUser(id, "")
	o.Busy(id, false)
	prompt("lost")
	o.readErr = core.ErrNotFound
	if err := svc.ObserveTurns(ctx); err != nil || len(armed()) != 0 || len(stops()) != 1 {
		t.Fatalf("lost session: armed=%v stops=%v err=%v", armed(), stops(), err)
	}
	if err := svc.ObserveTurns(ctx); err != nil || len(stops()) != 1 {
		t.Fatalf("repeated loss: %v", err)
	}
}

func TestTurnFakeOrca(t *testing.T) { orcatest.MaybeRun() }

func TestTurnThroughOrcaCLI(t *testing.T) {
	root := t.TempDir()
	db, err := store.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	f := servicetest.New()
	cfg := &config.Config{Workspaces: filepath.Join(root, "workspaces"), Agents: []config.Agent{{ID: "a", Command: []string{"agent"}}}, Clients: []config.Client{{ID: "c", Token: tokenA, Allow: []string{core.PermWorkspaceCreate, core.PermAgentRun}}}}
	cli := &orca.CLI{Command: orcatest.Command(t, "TestTurnFakeOrca", root)}
	deps := f.Deps(orcaws.New(cli, workspace.Folders{}, cfg.Workspaces))
	deps.Store = db
	deps.Now = time.Now
	deps.ObserveTurns = true
	svc := service.New(cfg, deps)
	defer svc.Close()
	c, _ := svc.Authenticate(tokenA)
	ctx := context.Background()
	op, err := svc.CreateWorkspace(ctx, c, service.CreateWorkspaceRequest{OperationID: "w", Name: "test", Note: "test"})
	if err != nil || op.State != core.StateSucceeded {
		t.Fatalf("workspace %v %v", op, err)
	}
	op, err = svc.StartAgentSession(ctx, c, service.StartAgentSessionRequest{OperationID: "s", Workspace: result[core.Workspace](t, op).ID, Agent: "a"})
	if err != nil || op.State != core.StateSucceeded {
		t.Fatalf("start %v %v", op, err)
	}
	session := result[core.AgentSession](t, op).ID
	op, err = svc.SendAgentPrompt(ctx, c, service.SendAgentPromptRequest{OperationID: "p", SessionID: session, Text: "hello"})
	if err != nil || op.State != core.StateSucceeded {
		t.Fatalf("prompt %v %v", op, err)
	}
	stops, err := db.PendingTurnStops()
	if err != nil || len(stops) != 1 || stops[0].SessionID != session || stops[0].State != core.AgentIdle {
		t.Fatalf("stops %v %v", stops, err)
	}
}

// The same loop the daemon starts must keep observing after its first read,
// and must resume an already armed turn on startup without a timer tick.
func TestRunningWatcherThroughPromptAndRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "watch.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { db.Close() }()
	f := servicetest.New()
	o := &controlledOrca{Orca: f.Orca, busyAfterSend: true}
	cfg := &config.Config{Workspaces: "/w", Agents: []config.Agent{{ID: "a", Command: []string{"agent"}}}, Clients: []config.Client{{ID: "c", Token: tokenA, Allow: []string{core.PermWorkspaceCreate, core.PermAgentRun}}}}
	deps := f.Deps(orcaws.New(o, f.Folders, "/w"))
	deps.Store, deps.Now, deps.ObserveTurns = db, time.Now, true
	svc := service.New(cfg, deps)
	defer func() { svc.Close() }()
	c, _ := svc.Authenticate(tokenA)
	op, err := svc.CreateWorkspace(ctx, c, service.CreateWorkspaceRequest{OperationID: "w", Name: "test", Note: "test"})
	if err != nil || op.State != core.StateSucceeded {
		t.Fatalf("workspace: %v %v", op, err)
	}
	op, err = svc.StartAgentSession(ctx, c, service.StartAgentSessionRequest{OperationID: "s", Workspace: result[core.Workspace](t, op).ID, Agent: "a"})
	if err != nil || op.State != core.StateSucceeded {
		t.Fatalf("session: %v %v", op, err)
	}
	id := result[core.AgentSession](t, op).ID
	prompt := func(opID string) {
		t.Helper()
		op, err := svc.SendAgentPrompt(ctx, c, service.SendAgentPromptRequest{OperationID: opID, SessionID: id, Text: "hello"})
		if err != nil || op.State != core.StateSucceeded {
			t.Fatalf("prompt: %v %v", op, err)
		}
	}
	start := func() (chan time.Time, context.CancelFunc, <-chan struct{}) {
		ticks := make(chan time.Time)
		watchCtx, cancel := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() { defer close(done); svc.WatchTurns(watchCtx, ticks) }()
		return ticks, cancel, done
	}
	tick := func(ch chan time.Time) {
		t.Helper()
		select {
		case ch <- time.Time{}:
		case <-time.After(3 * time.Second):
			t.Fatal("watcher did not accept scheduled read")
		}
	}
	waitStops := func(n int) []core.TurnStop {
		t.Helper()
		deadline := time.After(3 * time.Second)
		for {
			stops, err := db.PendingTurnStops()
			if err != nil {
				t.Fatal(err)
			}
			if len(stops) == n {
				return stops
			}
			select {
			case <-deadline:
				t.Fatalf("wanted %d stops, got %+v", n, stops)
			case <-time.After(time.Millisecond * 5):
			}
		}
	}
	prompt("p1")
	ticks, cancel, done := start()
	tick(ticks) // The startup read saw the working turn.
	tick(ticks) // The previous scheduled read also completed while working.
	if stops, err := db.PendingTurnStops(); err != nil || len(stops) != 0 {
		t.Fatalf("working: %v %v", stops, err)
	}
	o.AskUser(id, "permission")
	tick(ticks)
	stops := waitStops(1)
	if stops[0].SessionID != id || stops[0].State != core.AgentWaitingOnHuman {
		t.Fatalf("stop: %+v", stops)
	}
	tick(ticks)
	tick(ticks)
	if got := waitStops(1); got[0] != stops[0] {
		t.Fatalf("duplicate or changed stop: %v", got)
	}
	o.AskUser(id, "")
	o.Busy(id, false)
	prompt("p2")
	cancel()
	<-done
	svc.Close()
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	deps.Store = db
	svc = service.New(cfg, deps)
	o.AskUser(id, "permission")
	_, cancel, done = start()
	stops = waitStops(2) // No tick: the startup observation resumes the turn.
	cancel()
	<-done
	if stops[1].SessionID != id || stops[1].State != core.AgentWaitingOnHuman || stops[0].EventID == stops[1].EventID {
		t.Fatalf("restart: %+v", stops)
	}
	turns, err := db.ArmedTurns()
	if err != nil || len(turns) != 0 {
		t.Fatalf("still armed: %v %v", turns, err)
	}
}

type armFailureStore struct{ service.Store }

func (armFailureStore) ArmTurn(core.Turn) error { return errors.New("arm failed") }

func TestPendingReconciliationAndPromptFailures(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	f := servicetest.New()
	cfg := &config.Config{Workspaces: "/w", Agents: []config.Agent{{ID: "a", Command: []string{"agent"}}}, Clients: []config.Client{{ID: "c", Token: tokenA, Allow: []string{core.PermWorkspaceCreate, core.PermAgentRun}}}}
	deps := f.Deps(orcaws.New(f.Orca, f.Folders, "/w"))
	deps.Store = db
	deps.Now = time.Now
	svc := service.New(cfg, deps)
	defer svc.Close()
	c, _ := svc.Authenticate(tokenA)
	op, err := svc.CreateWorkspace(ctx, c, service.CreateWorkspaceRequest{OperationID: "w", Name: "test", Note: "test"})
	if err != nil {
		t.Fatal(err)
	}
	op, err = svc.StartAgentSession(ctx, c, service.StartAgentSessionRequest{OperationID: "s", Workspace: result[core.Workspace](t, op).ID, Agent: "a"})
	if err != nil {
		t.Fatal(err)
	}
	id := result[core.AgentSession](t, op).ID
	f.Orca.SilentAgent = true
	op, err = svc.SendAgentPrompt(ctx, c, service.SendAgentPromptRequest{OperationID: "failed", SessionID: id, Text: "hello"})
	if err != nil || op.State != core.StateFailed {
		t.Fatalf("failed prompt: %v %v", op, err)
	}
	f.Orca.SilentAgent = false
	deps.Store = armFailureStore{db}
	lost := service.New(cfg, deps)
	defer lost.Close()
	_, err = lost.SendAgentPrompt(ctx, c, service.SendAgentPromptRequest{OperationID: "lost", SessionID: id, Text: "hello"})
	var ce *core.Error
	if !errors.As(err, &ce) || ce.Code != core.CodeResultUnknown {
		t.Fatalf("arm failure: %v", err)
	}
	op, err = lost.GetOperation(c, service.GetOperationRequest{OperationID: "lost"})
	if err != nil || op.State != core.StateUnknown || op.ErrorCode != core.CodeResultUnknown {
		t.Fatalf("operation: %+v %v", op, err)
	}
	op, err = lost.SendAgentPrompt(ctx, c, service.SendAgentPromptRequest{OperationID: "lost", SessionID: id, Text: "hello"})
	if err != nil || op.State != core.StateUnknown {
		t.Fatalf("replay: %+v %v", op, err)
	}
	if err := db.BeginTurn(core.Turn{ClientID: "c", OperationID: "local-leftover", SessionID: "local"}); err != nil {
		t.Fatal(err)
	}
	if err := svc.ReconcilePendingTurns(ctx); err != nil {
		t.Fatal(err)
	}
	stops, err := db.PendingTurnStops()
	if err != nil || len(stops) != 2 {
		t.Fatalf("stops: %+v %v", stops, err)
	}
	seen := map[string]bool{}
	for _, stop := range stops {
		if stop.State != core.TurnUnknown || stop.EventID == "" || stop.ObservedAt.IsZero() {
			t.Fatalf("stop: %+v", stop)
		}
		seen[stop.SessionID] = true
	}
	if !seen[id] || !seen["local"] {
		t.Fatalf("sessions: %v", seen)
	}
	if err := svc.AckTurnStop(stops[0].EventID); err != nil {
		t.Fatal(err)
	}
	if err := svc.ReconcilePendingTurns(ctx); err != nil {
		t.Fatal(err)
	}
	stops, err = db.PendingTurnStops()
	if err != nil || len(stops) != 1 {
		t.Fatalf("repeated startup: %+v %v", stops, err)
	}
}

func TestPendingTurnStillWorkingIsArmedAtStartup(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	f := servicetest.New()
	o := &controlledOrca{Orca: f.Orca}
	cfg := &config.Config{Workspaces: "/w", Agents: []config.Agent{{ID: "a", Command: []string{"agent"}}}, Clients: []config.Client{{ID: "c", Token: tokenA, Allow: []string{core.PermWorkspaceCreate, core.PermAgentRun}}}}
	deps := f.Deps(orcaws.New(o, f.Folders, "/w"))
	deps.Store, deps.Now, deps.ObserveTurns = db, time.Now, true
	svc := service.New(cfg, deps)
	defer svc.Close()
	c, _ := svc.Authenticate(tokenA)
	op, err := svc.CreateWorkspace(ctx, c, service.CreateWorkspaceRequest{OperationID: "w", Name: "test", Note: "test"})
	if err != nil {
		t.Fatal(err)
	}
	op, err = svc.StartAgentSession(ctx, c, service.StartAgentSessionRequest{OperationID: "s", Workspace: result[core.Workspace](t, op).ID, Agent: "a"})
	if err != nil {
		t.Fatal(err)
	}
	id := result[core.AgentSession](t, op).ID
	reconcile := func(s *service.Service, opID string) (core.TurnStop, []core.Turn) {
		t.Helper()
		turn := core.Turn{ClientID: "c", OperationID: opID, SessionID: id}
		if err := db.BeginTurn(turn); err != nil {
			t.Fatal(err)
		}
		if err := s.ReconcilePendingTurns(ctx); err != nil {
			t.Fatal(err)
		}
		stops, err := db.PendingTurnStops()
		if err != nil || len(stops) != 1 || stops[0].State != core.TurnUnknown || stops[0].SessionID != id {
			t.Fatalf("%s stops: %+v %v", opID, stops, err)
		}
		if err := db.DeleteTurnStop(stops[0].EventID); err != nil {
			t.Fatal(err)
		}
		if pending, err := db.PendingTurns(); err != nil || len(pending) != 0 {
			t.Fatalf("%s pending: %v %v", opID, pending, err)
		}
		armed, err := db.ArmedTurns()
		if err != nil {
			t.Fatal(err)
		}
		return stops[0], armed
	}

	o.Busy(id, true)
	if _, armed := reconcile(svc, "working"); len(armed) != 1 || armed[0].OperationID != "working" {
		t.Fatalf("working turn not armed: %v", armed)
	}
	o.Busy(id, false)
	if err := svc.ObserveTurns(ctx); err != nil {
		t.Fatal(err)
	}
	stops, err := db.PendingTurnStops()
	if err != nil || len(stops) != 1 || stops[0].State != core.AgentIdle || stops[0].SessionID != id {
		t.Fatalf("stop after rearm: %+v %v", stops, err)
	}
	if err := db.DeleteTurnStop(stops[0].EventID); err != nil {
		t.Fatal(err)
	}

	if _, armed := reconcile(svc, "idle"); len(armed) != 0 {
		t.Fatalf("idle session armed: %v", armed)
	}
	o.Busy(id, true)
	o.readErr = errors.New("orca unavailable")
	if _, armed := reconcile(svc, "unreadable"); len(armed) != 0 {
		t.Fatalf("unreadable session armed: %v", armed)
	}
	o.readErr = nil
	current := core.Turn{ClientID: "c", OperationID: "current", SessionID: id}
	if err := db.BeginTurn(current); err != nil {
		t.Fatal(err)
	}
	if err := db.ArmTurn(current); err != nil {
		t.Fatal(err)
	}
	if _, armed := reconcile(svc, "behind"); len(armed) != 1 || armed[0] != current {
		t.Fatalf("second turn armed for one session: %v", armed)
	}
	if err := svc.ObserveTurns(ctx); err != nil {
		t.Fatal(err)
	}
	if err := db.DisarmTurn(current); err != nil {
		t.Fatal(err)
	}
	deps.ObserveTurns = false
	unobserved := service.New(cfg, deps)
	defer unobserved.Close()
	if _, armed := reconcile(unobserved, "unobserved"); len(armed) != 0 {
		t.Fatalf("armed without observation: %v", armed)
	}
}
