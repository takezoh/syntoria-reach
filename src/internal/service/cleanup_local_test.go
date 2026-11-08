package service_test

import (
	"context"
	"testing"
	"time"

	"github.com/takezoh/syntoria-reach/src/internal/config"
	"github.com/takezoh/syntoria-reach/src/internal/core"
	"github.com/takezoh/syntoria-reach/src/internal/localws"
	"github.com/takezoh/syntoria-reach/src/internal/service"
	"github.com/takezoh/syntoria-reach/src/internal/service/servicetest"
)

// Cleanup on a node without Orca: a completed local workspace without
// activity for core.CleanupAfter goes, record and folder; one with an open
// session stays until the session is stopped; a folder without a record
// goes once unchanged for core.CleanupAfter.
func TestCleanupLocalWorkspaces(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	old := now.Add(-core.CleanupAfter - time.Hour)
	f := servicetest.New()
	cfg := &config.Config{
		Workspaces: "/w",
		Agents:     []config.Agent{{ID: "coder", Role: "Writes code", Command: []string{"/bin/coder"}, Protocol: core.ProtocolClaude}},
		Clients:    []config.Client{{ID: "a", Token: tokenA, Allow: []string{core.PermWorkspaceCreate, core.PermAgentRun}}},
	}
	svc := service.New(cfg, f.Deps(localws.New("/w", f.Records, f.Folders, f.Agents, servicetest.NewIDs())))
	t.Cleanup(svc.Close)
	a, _ := svc.Authenticate(tokenA)

	f.Records.Now = func() time.Time { return old }
	f.Store.Now = func() time.Time { return old }
	create := func(name string) string {
		op, err := svc.CreateWorkspace(ctx, a, service.CreateWorkspaceRequest{OperationID: "c-" + name, Name: name, Note: "n"})
		if err != nil || op.State != core.StateSucceeded {
			t.Fatalf("create %s: %+v %v", name, op, err)
		}
		id := result[core.Workspace](t, op).ID
		if op, _ := svc.UpdateWorkspace(ctx, a, service.UpdateWorkspaceRequest{OperationID: "u-" + name, Workspace: id, Status: "completed"}); op.State != core.StateSucceeded {
			t.Fatalf("complete %s: %+v", name, op)
		}
		return id
	}
	done, running := create("done"), create("running")
	op, _ := svc.StartAgentSession(ctx, a, service.StartAgentSessionRequest{OperationID: "s", Workspace: running, Agent: "coder"})
	session := result[core.AgentSession](t, op).ID
	f.Folders.Make("/w/reach-stray") // left by a daemon that stopped while creating it
	f.Folders.Change("/w/reach-stray", old)
	f.Records.Now, f.Store.Now = nil, nil

	removed, err := svc.Cleanup(ctx, now)
	if err != nil || len(removed) != 1 || removed[0] != done {
		t.Fatalf("removed %q %v", removed, err)
	}
	if f.Folders.Exists("/w/reach-done") || f.Folders.Exists("/w/reach-stray") || !f.Folders.Exists("/w/reach-running") {
		t.Fatal("folders: the done and stray ones must go, the running one stay")
	}
	if _, err := f.Records.Workspace(done); err == nil {
		t.Fatal("the record of a removed workspace stayed")
	}
	// Starting the agent set the workspace back in progress; once completed
	// again and its session stopped, it is due.
	svc.UpdateWorkspace(ctx, a, service.UpdateWorkspaceRequest{OperationID: "u-again", Workspace: running, Status: "completed"})
	svc.StopAgentSession(ctx, a, service.StopAgentSessionRequest{OperationID: "x", SessionID: session})
	if removed, _ := svc.Cleanup(ctx, now.Add(core.CleanupAfter+time.Hour)); len(removed) != 1 || removed[0] != running {
		t.Fatalf("after the session stopped: %q", removed)
	}
}
