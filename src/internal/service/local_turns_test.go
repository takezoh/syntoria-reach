package service_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

type failingTurnStore struct{ service.Store }

func (s failingTurnStore) CloseTurn(core.Turn, core.TurnStop) error {
	return errors.New("disk failure")
}

func TestLocalTurnFake(t *testing.T) { agenttest.MaybeRun() }

func TestLocalTurnThroughPrompt(t *testing.T) {
	for _, tc := range []struct {
		name, protocol, text, state string
		stop                        bool
	}{
		{"completion", "codex", "secret reply", core.AgentIdle, false},
		{"error", "claude", "fail", core.AgentIdle, false},
		{"invalid protocol", "codex", "invalid-json", core.AgentIdle, false},
		{"closed stdout", "codex", "close-stdout", core.AgentIdle, false},
		{"exit", "codex", "crash", core.AgentExited, false},
		{"stop", "codex", "hang", core.AgentExited, true},
		{"stop persistence failure", "codex", "hang", core.AgentExited, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			db, err := store.Open(filepath.Join(root, "state.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			cmd, env := agenttest.Agent(t, tc.protocol, "TestLocalTurnFake")
			cfg := &config.Config{Workspaces: filepath.Join(root, "workspaces"), Agents: []config.Agent{{ID: "a", Protocol: tc.protocol, Command: cmd, Env: env}}, Clients: []config.Client{{ID: "c", Token: tokenA, Allow: []string{core.PermWorkspaceCreate, core.PermAgentRun}}}}
			f := servicetest.New()
			runtime := agent.New(filepath.Join(root, "logs"))
			defer runtime.Close()
			deps := f.Deps(localws.New(cfg.Workspaces, db, workspace.Folders{}, runtime, f.Deps(nil).NewID))
			deps.Store = db
			if tc.name == "stop persistence failure" {
				deps.Store = failingTurnStore{db}
			}
			deps.Now = time.Now
			svc := service.New(cfg, deps)
			defer svc.Close()
			c, _ := svc.Authenticate(tokenA)
			ctx := context.Background()
			op, err := svc.CreateWorkspace(ctx, c, service.CreateWorkspaceRequest{OperationID: "w", Name: "test", Note: "test"})
			if err != nil || op.State != core.StateSucceeded {
				t.Fatalf("workspace: %v %v", op, err)
			}
			op, err = svc.StartAgentSession(ctx, c, service.StartAgentSessionRequest{OperationID: "s", Workspace: result[core.Workspace](t, op).ID, Agent: "a"})
			if err != nil || op.State != core.StateSucceeded {
				t.Fatalf("start: %v %v", op, err)
			}
			id := result[core.AgentSession](t, op).ID
			op, err = svc.SendAgentPrompt(ctx, c, service.SendAgentPromptRequest{OperationID: "p", SessionID: id, Text: tc.text})
			if err != nil || op.State != core.StateSucceeded {
				t.Fatalf("prompt: %v %v", op, err)
			}
			if tc.stop {
				op, err = svc.StopAgentSession(ctx, c, service.StopAgentSessionRequest{OperationID: "stop", SessionID: id})
				if tc.name == "stop persistence failure" {
					// A failed record must not keep the agent from stopping.
					if err != nil || op.State != core.StateSucceeded {
						t.Fatalf("stop: %v %v", op, err)
					}
					// The turn stays armed and nothing is recorded.
					armed, e := db.ArmedTurns()
					if e != nil || len(armed) != 1 {
						t.Fatalf("armed: %v %v", armed, e)
					}
					stops, e := db.PendingTurnStops()
					if e != nil || len(stops) != 0 {
						t.Fatalf("stops: %v %v", stops, e)
					}
					// The session is gone, so the watcher disarms the turn
					// without forging a stop.
					if e := svc.ObserveTurns(ctx); e != nil {
						t.Fatal(e)
					}
					if armed, e = db.ArmedTurns(); e != nil || len(armed) != 0 {
						t.Fatalf("still armed: %v %v", armed, e)
					}
					if stops, e = db.PendingTurnStops(); e != nil || len(stops) != 0 {
						t.Fatalf("forged stop: %v %v", stops, e)
					}
					return
				}
				if err != nil || op.State != core.StateSucceeded {
					t.Fatalf("stop: %v %v", op, err)
				}
			}
			deadline := time.Now().Add(3 * time.Second)
			for {
				stops, e := db.PendingTurnStops()
				if e != nil {
					t.Fatal(e)
				}
				armed, e := db.ArmedTurns()
				if e != nil {
					t.Fatal(e)
				}
				if len(stops) == 1 && len(armed) == 0 {
					if (stops[0].State != tc.state && !(tc.name == "exit" && stops[0].State == core.AgentIdle)) || stops[0].SessionID != id {
						t.Fatalf("stop: %+v", stops)
					}
					encoded, e := json.Marshal(stops[0])
					if e != nil {
						t.Fatal(e)
					}
					if strings.Contains(string(encoded), "secret reply") {
						t.Fatalf("reply leaked: %s", encoded)
					}
					if tc.name == "invalid protocol" || tc.name == "closed stdout" {
						for {
							out, e := svc.ReadAgentOutput(ctx, c, service.ReadAgentOutputRequest{SessionID: id})
							if e != nil {
								t.Fatal(e)
							}
							expected := "invalid message from harness"
							if tc.name == "closed stdout" {
								expected = "harness connection closed"
							}
							if out.State == core.AgentIdle && strings.Contains(out.Reply, expected) {
								break
							}
							if time.Now().After(deadline) {
								t.Fatalf("error reply unavailable: %+v", out)
							}
							time.Sleep(5 * time.Millisecond)
						}
						op, e := svc.StopAgentSession(ctx, c, service.StopAgentSessionRequest{OperationID: "after-error", SessionID: id})
						if e != nil || op.State != core.StateSucceeded {
							t.Fatalf("stop after error: %v %v", op, e)
						}
						after, e := db.PendingTurnStops()
						if e != nil || len(after) != 1 || after[0] != stops[0] {
							t.Fatalf("later exit changed stop: %v %v", after, e)
						}
					}
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("stops=%+v armed=%+v", stops, armed)
				}
				time.Sleep(5 * time.Millisecond)
			}
		})
	}
}

func TestLocalLostTurnAfterRestart(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "state.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	turn := core.Turn{ClientID: "c", OperationID: "p", SessionID: "gone"}
	if err = db.BeginTurn(turn); err != nil {
		t.Fatal(err)
	}
	if err = db.ArmTurn(turn); err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	f := servicetest.New()
	runtime := agent.New(filepath.Join(root, "logs"))
	defer runtime.Close()
	deps := f.Deps(localws.New(filepath.Join(root, "workspaces"), db, workspace.Folders{}, runtime, f.Deps(nil).NewID))
	deps.Store = db
	svc := service.New(&config.Config{}, deps)
	defer svc.Close()
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	defer slog.SetDefault(previous)
	for i := 0; i < 2; i++ {
		if err = svc.ObserveTurns(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	armed, err := db.ArmedTurns()
	if err != nil || len(armed) != 0 {
		t.Fatalf("armed: %v %v", armed, err)
	}
	if n := strings.Count(logs.String(), "armed turn session lost"); n != 1 {
		t.Fatalf("loss logs: %s", logs.String())
	}
	stops, err := db.PendingTurnStops()
	if err != nil || len(stops) != 0 {
		t.Fatalf("stops: %v %v", stops, err)
	}
}

func TestLocalArmFailureReconcilesOnRestart(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "state.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { db.Close() }()
	cmd, env := agenttest.Agent(t, "codex", "TestLocalTurnFake")
	cfg := &config.Config{Workspaces: filepath.Join(root, "workspaces"), Agents: []config.Agent{{ID: "a", Protocol: "codex", Command: cmd, Env: env}}, Clients: []config.Client{{ID: "c", Token: tokenA, Allow: []string{core.PermWorkspaceCreate, core.PermAgentRun}}}}
	f := servicetest.New()
	runtime := agent.New(filepath.Join(root, "logs"))
	defer runtime.Close()
	deps := f.Deps(localws.New(cfg.Workspaces, db, workspace.Folders{}, runtime, f.Deps(nil).NewID))
	deps.Store = armFailureStore{db}
	deps.Now = time.Now
	svc := service.New(cfg, deps)
	defer svc.Close()
	c, _ := svc.Authenticate(tokenA)
	ctx := context.Background()
	op, err := svc.CreateWorkspace(ctx, c, service.CreateWorkspaceRequest{OperationID: "w", Name: "test", Note: "test"})
	if err != nil {
		t.Fatal(err)
	}
	op, err = svc.StartAgentSession(ctx, c, service.StartAgentSessionRequest{OperationID: "s", Workspace: result[core.Workspace](t, op).ID, Agent: "a"})
	if err != nil {
		t.Fatal(err)
	}
	id := result[core.AgentSession](t, op).ID
	_, err = svc.SendAgentPrompt(ctx, c, service.SendAgentPromptRequest{OperationID: "p", SessionID: id, Text: "hang"})
	var ce *core.Error
	if !errors.As(err, &ce) || ce.Code != core.CodeResultUnknown {
		t.Fatalf("prompt: %v", err)
	}
	op, err = svc.GetOperation(c, service.GetOperationRequest{OperationID: "p"})
	if err != nil || op.State != core.StateUnknown || op.ErrorCode != core.CodeResultUnknown {
		t.Fatalf("operation: %+v %v", op, err)
	}
	svc.Close()
	runtime.Close()
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	deps.Store = db
	restarted := service.New(cfg, deps)
	defer restarted.Close()
	if err := restarted.ReconcilePendingTurns(ctx); err != nil {
		t.Fatal(err)
	}
	stops, err := db.PendingTurnStops()
	if err != nil || len(stops) != 1 || stops[0].State != core.TurnUnknown || stops[0].SessionID != id {
		t.Fatalf("stops: %+v %v", stops, err)
	}
	if err := restarted.AckTurnStop(stops[0].EventID); err != nil {
		t.Fatal(err)
	}
	if err := restarted.ReconcilePendingTurns(ctx); err != nil {
		t.Fatal(err)
	}
	stops, err = db.PendingTurnStops()
	if err != nil || len(stops) != 0 {
		t.Fatalf("repeat: %+v %v", stops, err)
	}
}
