package service

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/takezoh/syntoria-reach/src/internal/orca/orcatest"
	"github.com/takezoh/syntoria-reach/src/internal/store"
)

func TestCreateWorkspace(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	projects := f.svc.ListProjects(f.a)
	if len(projects) != 1 || projects[0].ID != "proj" || len(projects[0].Agents) != 1 {
		t.Fatalf("projects = %+v", projects)
	}
	if _, err := f.svc.CreateWorkspace(ctx, f.a, CreateWorkspaceRequest{OperationID: "c", Project: "nope", Name: "x"}); code(err) != CodePermissionDenied {
		t.Fatalf("unknown project: %v", err)
	}
	if _, err := f.svc.CreateWorkspace(ctx, f.a, CreateWorkspaceRequest{OperationID: "c", Project: "proj", Name: "../x"}); code(err) != CodeInvalidRequest {
		t.Fatalf("bad name: %v", err)
	}

	// Resending the create returns the same workspace.
	again, err := f.svc.CreateWorkspace(ctx, f.a, CreateWorkspaceRequest{OperationID: "create-ws", Project: "proj", Name: "ws"})
	if err != nil || !strings.Contains(string(again.Result), f.ws) {
		t.Fatalf("resend: %+v %v", again, err)
	}
	if n := strings.Count(orcatest.Calls(t, f.orcaRoot), "worktree create"); n != 1 {
		t.Fatalf("orca create called %d times", n)
	}

	// An Orca failure is recorded and leaves no usable workspace.
	op, err := f.svc.CreateWorkspace(ctx, f.a, CreateWorkspaceRequest{OperationID: "c-fail", Project: "proj", Name: "fail"})
	if err != nil || op.State != store.StateFailed || op.ErrorCode != CodeOrcaFailed {
		t.Fatalf("failed create: %+v %v", op, err)
	}
	list, _ := f.svc.ListWorkspaces(f.a)
	if len(list) != 1 || list[0].ID != f.ws || list[0].Path != f.root {
		t.Fatalf("workspaces = %+v", list)
	}
	if list, _ := f.svc.ListWorkspaces(f.b); len(list) != 0 {
		t.Fatalf("other client sees workspaces: %+v", list)
	}
}

func TestDeleteWorkspace(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	del := func(opID string) *store.Operation {
		t.Helper()
		op, err := f.svc.DeleteWorkspace(ctx, f.a, WorkspaceRequest{OperationID: opID, Workspace: f.ws})
		if err != nil {
			t.Fatal(err)
		}
		return op
	}
	if _, err := f.svc.DeleteWorkspace(ctx, f.b, WorkspaceRequest{OperationID: "d", Workspace: f.ws}); code(err) != CodeNotFound {
		t.Fatalf("other client deletes: %v", err)
	}

	// Not while a job runs.
	jobOp, _ := f.svc.StartJob(f.a, StartJobRequest{OperationID: "j", Workspace: f.ws, Executable: "/bin/sh", Args: []string{"-c", "sleep 30"}})
	var j store.Job
	json.Unmarshal(jobOp.Result, &j)
	if op := del("d1"); op.State != store.StateFailed || op.ErrorCode != CodeInvalidState {
		t.Fatalf("delete while busy: %+v", op)
	}
	f.svc.CancelJob(f.a, CancelJobRequest{OperationID: "cj", JobID: j.ID})

	// Not when Orca refuses (uncommitted changes); the workspace stays usable.
	f.svc.WriteFile(f.a, WriteFileRequest{OperationID: "w", Workspace: f.ws, Path: "dirty", Content: []byte("x")})
	if op := del("d2"); op.State != store.StateFailed || op.ErrorCode != CodeOrcaFailed {
		t.Fatalf("delete dirty: %+v", op)
	}
	if _, err := f.svc.ReadFile(f.a, ReadFileRequest{Workspace: f.ws, Path: "dirty"}); err != nil {
		t.Fatalf("workspace unusable after failed delete: %v", err)
	}

	os.Remove(filepath.Join(f.root, "dirty"))
	op := del("d3")
	var w store.Workspace
	json.Unmarshal(op.Result, &w)
	if op.State != store.StateSucceeded || w.State != store.WorkspaceDeleted {
		t.Fatalf("delete: %+v", op)
	}
	if _, err := os.Stat(f.root); !os.IsNotExist(err) {
		t.Fatal("worktree still exists")
	}
	if !strings.Contains(orcatest.Calls(t, f.orcaRoot), "--delete-branch") {
		t.Fatal("branch deletion not requested")
	}
	if _, err := f.svc.ReadFile(f.a, ReadFileRequest{Workspace: f.ws, Path: "x"}); code(err) != CodePermissionDenied {
		t.Fatalf("deleted workspace still usable: %v", err)
	}
	if list, _ := f.svc.ListWorkspaces(f.a); len(list) != 0 {
		t.Fatalf("deleted workspace listed: %+v", list)
	}
}
