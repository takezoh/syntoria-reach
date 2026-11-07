package service_test

import (
	"context"
	"strings"
	"testing"

	"github.com/takezoh/syntoria-reach/src/internal/core"
	"github.com/takezoh/syntoria-reach/src/internal/service"
)

func TestCreateWorkspace(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	projects := f.svc.ListProjects(f.a)
	if len(projects) != 1 || projects[0].ID != "proj" || len(projects[0].Agents) != 2 || len(projects[0].Commands) != 1 {
		t.Fatalf("projects = %+v", projects)
	}
	// Commands and agents are listed only with the permission to use them.
	if p := f.svc.ListProjects(f.b); len(p) != 1 || p[0].Commands != nil || p[0].Agents != nil {
		t.Fatalf("projects of b = %+v", p)
	}
	if _, err := f.svc.CreateWorkspace(ctx, f.a, service.CreateWorkspaceRequest{OperationID: "c", Project: "nope", Name: "x"}); code(err) != core.CodePermissionDenied {
		t.Fatalf("unknown project: %v", err)
	}
	if _, err := f.svc.CreateWorkspace(ctx, f.a, service.CreateWorkspaceRequest{OperationID: "c", Project: "proj", Name: "../x"}); code(err) != core.CodeInvalidRequest {
		t.Fatalf("bad name: %v", err)
	}

	// Resending the create returns the same workspace.
	again, err := f.svc.CreateWorkspace(ctx, f.a, service.CreateWorkspaceRequest{OperationID: "create-ws", Project: "proj", Name: "ws"})
	if err != nil || result[core.Workspace](t, again).ID != f.ws {
		t.Fatalf("resend: %+v %v", again, err)
	}
	if calls := f.fakes.Orca.Calls(); len(calls) != 1 || calls[0] != "create /repo ws" {
		t.Fatalf("orca calls = %q", calls)
	}

	// An Orca failure is recorded and leaves no usable workspace.
	f.fakes.Orca.FailCreate = map[string]bool{"fail": true}
	op, err := f.svc.CreateWorkspace(ctx, f.a, service.CreateWorkspaceRequest{OperationID: "c-fail", Project: "proj", Name: "fail"})
	if err != nil || op.State != core.StateFailed || op.ErrorCode != core.CodeOrcaFailed || !strings.Contains(op.ErrorMessage, "git_failed") {
		t.Fatalf("failed create: %+v %v", op, err)
	}
	if w, _ := f.fakes.Store.GetWorkspace("a", "ws_2"); w == nil || w.State != core.WorkspaceFailed {
		t.Fatalf("failed workspace = %+v", w)
	}
	list, _ := f.svc.ListWorkspaces(f.a)
	if len(list) != 1 || list[0].ID != f.ws || list[0].Path != f.root || list[0].Branch != "refs/heads/ws" {
		t.Fatalf("workspaces = %+v", list)
	}
	if list, _ := f.svc.ListWorkspaces(f.b); len(list) != 0 {
		t.Fatalf("other client sees workspaces: %+v", list)
	}
}

func TestDeleteWorkspace(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	del := func(opID string) *core.Operation {
		t.Helper()
		op, err := f.svc.DeleteWorkspace(ctx, f.a, service.WorkspaceRequest{OperationID: opID, Workspace: f.ws})
		if err != nil {
			t.Fatal(err)
		}
		return op
	}
	if _, err := f.svc.DeleteWorkspace(ctx, f.b, service.WorkspaceRequest{OperationID: "d", Workspace: f.ws}); code(err) != core.CodeNotFound {
		t.Fatalf("other client deletes: %v", err)
	}

	// Not while a job runs or an agent session is active.
	j := f.startJob(t, "j")
	if op := del("d1"); op.State != core.StateFailed || op.ErrorCode != core.CodeInvalidState {
		t.Fatalf("delete with a running job: %+v", op)
	}
	f.fakes.Jobs.Exit(j.ID, core.JobExit{})
	s := f.startSession(t, "s", service.StartAgentSessionRequest{})
	if op := del("d2"); op.State != core.StateFailed || op.ErrorCode != core.CodeInvalidState {
		t.Fatalf("delete with an active session: %+v", op)
	}
	f.svc.StopAgentSession(f.a, service.StopAgentSessionRequest{OperationID: "x", SessionID: s.ID})

	// Not when Orca refuses (uncommitted changes); the workspace stays usable.
	f.svc.WriteFile(f.a, service.WriteFileRequest{OperationID: "w", Workspace: f.ws, Path: "dirty", Content: []byte("x")})
	f.fakes.Orca.FailRemove = map[string]bool{"ws": true}
	if op := del("d3"); op.State != core.StateFailed || op.ErrorCode != core.CodeOrcaFailed {
		t.Fatalf("delete dirty: %+v", op)
	}
	if _, err := f.svc.ReadFile(f.a, service.ReadFileRequest{Workspace: f.ws, Path: "dirty"}); err != nil {
		t.Fatalf("workspace unusable after failed delete: %v", err)
	}

	f.fakes.Orca.FailRemove = nil
	op := del("d4")
	if w := result[core.Workspace](t, op); op.State != core.StateSucceeded || w.State != core.WorkspaceDeleted {
		t.Fatalf("delete: %+v", op)
	}
	if wts := f.fakes.Orca.Worktrees(); len(wts) != 0 {
		t.Fatalf("worktrees left: %q", wts)
	}
	if n := f.fakes.Files.OpenRoots(f.root); n != 0 {
		t.Fatalf("deleted workspace still has %d open roots", n)
	}
	if _, err := f.svc.ReadFile(f.a, service.ReadFileRequest{Workspace: f.ws, Path: "x"}); code(err) != core.CodePermissionDenied {
		t.Fatalf("deleted workspace still usable: %v", err)
	}
	if list, _ := f.svc.ListWorkspaces(f.a); len(list) != 0 {
		t.Fatalf("deleted workspace listed: %+v", list)
	}
	if op := del("d5"); op.State != core.StateFailed || op.ErrorCode != core.CodeInvalidState {
		t.Fatalf("second delete: %+v", op)
	}
}
