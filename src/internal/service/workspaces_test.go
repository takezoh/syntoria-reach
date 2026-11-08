package service_test

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/takezoh/syntoria-reach/src/internal/core"
	"github.com/takezoh/syntoria-reach/src/internal/service"
)

func TestCapabilities(t *testing.T) {
	f := newFixture(t)
	if c := f.svc.ListCapabilities(f.a); len(c.Permissions) != 5 || !slices.Equal(c.Commands, []string{"/bin/sh"}) || !slices.Equal(c.Agents, []string{"codex", "dsh"}) {
		t.Fatalf("capabilities of a = %+v", c)
	}
	// Commands and agents are listed only with the permission to run them.
	if c := f.svc.ListCapabilities(f.b); len(c.Permissions) != 2 || c.Commands != nil || c.Agents != nil {
		t.Fatalf("capabilities of b = %+v", c)
	}
}

func TestCreateWorkspace(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	for name, c := range map[string]struct {
		req  service.CreateWorkspaceRequest
		code string
	}{
		"bad name":   {service.CreateWorkspaceRequest{Name: "../x", Note: "n"}, core.CodeInvalidRequest},
		"no note":    {service.CreateWorkspaceRequest{Name: "x"}, core.CodeInvalidRequest},
		"blank note": {service.CreateWorkspaceRequest{Name: "x", Note: " \n"}, core.CodeInvalidRequest},
	} {
		c.req.OperationID = "c"
		if _, err := f.svc.CreateWorkspace(ctx, f.a, c.req); code(err) != c.code {
			t.Errorf("%s: %v", name, err)
		}
	}

	// The fixture's workspace: a folder under the workspaces folder,
	// registered in Orca as a folder project named with the prefix, with the
	// note on its root entry.
	if !f.fakes.Folders.Exists(f.root) {
		t.Fatal("no workspace folder")
	}
	if calls := f.fakes.Orca.Calls(); strings.Join(calls, "|") != "add /w/reach-ws|repo "+f.ws+"|worktrees "+f.ws+"|set "+f.ws+"::/w/reach-ws work on ws " {
		t.Fatalf("orca calls = %q", calls)
	}
	// Resending the create returns the same workspace without doing anything.
	again, err := f.svc.CreateWorkspace(ctx, f.a, service.CreateWorkspaceRequest{OperationID: "create-ws", Name: "ws", Note: "work on ws"})
	w := result[core.Workspace](t, again)
	if err != nil || w != (core.Workspace{ID: f.ws, Name: "reach-ws", Note: "work on ws", Status: "in-progress", Path: f.root}) {
		t.Fatalf("resend: %+v %v", again, err)
	}
	if n := len(f.fakes.Orca.Calls()); n != 4 {
		t.Fatalf("the resend called Orca: %d calls", n)
	}
	// A name that already has the prefix keeps it once.
	if id := f.newWorkspace(t, f.a, "reach-second"); f.fakes.Orca.Root(id).Path != "/w/reach-second" {
		t.Fatalf("prefixed name: %+v", f.fakes.Orca.Root(id))
	}

	// The same name again: the folder exists.
	if op, _ := f.svc.CreateWorkspace(ctx, f.a, service.CreateWorkspaceRequest{OperationID: "c-dup", Name: "ws", Note: "n"}); op.State != core.StateFailed || op.ErrorCode != core.CodeInvalidState {
		t.Fatalf("duplicate name: %+v", op)
	}
	// Orca refusing leaves no folder behind.
	f.fakes.Orca.FailAdd = map[string]bool{"reach-fail": true}
	op, err := f.svc.CreateWorkspace(ctx, f.a, service.CreateWorkspaceRequest{OperationID: "c-fail", Name: "fail", Note: "n"})
	if err != nil || op.State != core.StateFailed || op.ErrorCode != core.CodeOrcaFailed || !strings.Contains(op.ErrorMessage, "add_failed") {
		t.Fatalf("failed create: %+v %v", op, err)
	}
	if f.fakes.Folders.Exists("/w/reach-fail") {
		t.Fatal("the folder of a refused workspace was left")
	}
}

func TestListWorkspaces(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	second := f.newWorkspace(t, f.c, "another")
	// Orca projects count only with the prefix.
	f.fakes.Orca.AddRepo(core.Repo{ID: "git-1", DisplayName: "syntoria-reach", Path: "/src/syntoria-reach", Kind: "git"})

	list, err := f.svc.ListWorkspaces(ctx, f.a)
	if err != nil || len(list) != 2 || list[0].ID != second || list[1].ID != f.ws {
		t.Fatalf("workspaces = %+v %v", list, err)
	}
	if *list[1] != (core.Workspace{ID: f.ws, Name: "reach-ws", Note: "work on ws", Status: "in-progress", Path: f.root}) {
		t.Fatalf("workspace = %+v", list[1])
	}
	// Workspaces are not owned by the client that made them.
	if list, _ := f.svc.ListWorkspaces(ctx, f.b); len(list) != 2 {
		t.Fatalf("b sees %+v", list)
	}

	// A workspace removed in Orca is gone from Reach too.
	f.fakes.Orca.DeleteProject(ctx, second)
	if list, _ := f.svc.ListWorkspaces(ctx, f.a); len(list) != 1 {
		t.Fatalf("after removal in Orca: %+v", list)
	}
	if _, err := f.svc.ReadFile(ctx, f.a, service.ReadFileRequest{Workspace: second, Path: "x"}); code(err) != core.CodeNotFound {
		t.Fatalf("removed workspace: %v", err)
	}

	// Orca being down is an error, never an empty list.
	f.fakes.Orca.FailList = true
	if _, err := f.svc.ListWorkspaces(ctx, f.a); code(err) != core.CodeOrcaFailed {
		t.Fatalf("Orca down: %v", err)
	}
	if _, err := f.svc.ReadFile(ctx, f.a, service.ReadFileRequest{Workspace: f.ws, Path: "x"}); code(err) != core.CodeOrcaFailed {
		t.Fatalf("Orca down on a file read: %v", err)
	}
}

func TestUpdateWorkspace(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	update := func(opID, note, status string) *core.Operation {
		t.Helper()
		op, err := f.svc.UpdateWorkspace(ctx, f.c, service.UpdateWorkspaceRequest{OperationID: opID, Workspace: f.ws, Note: note, Status: status})
		if err != nil {
			t.Fatal(err)
		}
		return op
	}
	for name, req := range map[string]service.UpdateWorkspaceRequest{
		"nothing to change": {Workspace: f.ws},
		"bad status":        {Workspace: f.ws, Status: "In Review"},
		"blank note":        {Workspace: f.ws, Note: " "},
	} {
		req.OperationID = "u"
		if _, err := f.svc.UpdateWorkspace(ctx, f.a, req); code(err) != core.CodeInvalidRequest {
			t.Errorf("%s: %v", name, err)
		}
	}

	// Note and status change in Orca, separately or together.
	if w := result[core.Workspace](t, update("u1", "", "in-review")); w.Status != "in-review" || w.Note != "work on ws" {
		t.Fatalf("status: %+v", w)
	}
	if w := result[core.Workspace](t, update("u2", "ws: tests added", "")); w.Status != "in-review" || w.Note != "ws: tests added" {
		t.Fatalf("note: %+v", w)
	}
	if root := f.fakes.Orca.Root(f.ws); root.Comment != "ws: tests added" || root.WorkspaceStatus != "in-review" {
		t.Fatalf("Orca has %+v", root)
	}
	// Orca decides which statuses exist.
	if op := update("u3", "", "archived-x"); op.State != core.StateFailed || op.ErrorCode != core.CodeOrcaFailed {
		t.Fatalf("unknown status: %+v", op)
	}
	f.fakes.Orca.DeleteProject(ctx, f.ws)
	if op := update("u4", "x", ""); op.State != core.StateFailed || op.ErrorCode != core.CodeNotFound {
		t.Fatalf("missing workspace: %+v", op)
	}
}

func TestStartingAnAgentReopensTheWorkspace(t *testing.T) {
	f := newFixture(t)
	f.svc.UpdateWorkspace(context.Background(), f.a, service.UpdateWorkspaceRequest{OperationID: "u", Workspace: f.ws, Status: "completed"})
	f.startSession(t, "s", service.StartAgentSessionRequest{})
	if root := f.fakes.Orca.Root(f.ws); root.WorkspaceStatus != core.WorkspaceInProgress {
		t.Fatalf("status after starting an agent: %s", root.WorkspaceStatus)
	}
}

func TestCleanup(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	now := time.Now()
	old := now.Add(-core.CleanupAfter - time.Hour)
	at := func(t time.Time) { f.fakes.Store.Now = func() time.Time { return t } }
	complete := func(id string) {
		f.svc.UpdateWorkspace(ctx, f.a, service.UpdateWorkspaceRequest{OperationID: "done-" + id, Workspace: id, Status: "completed"})
	}

	at(old)
	done := f.newWorkspace(t, f.a, "done")       // completed, nothing since
	recent := f.newWorkspace(t, f.a, "recent")   // completed long ago, used through Reach since
	running := f.newWorkspace(t, f.a, "running") // completed, but an agent session is active
	inProgress := f.newWorkspace(t, f.a, "open") // not completed
	f.startSession(t, "s", service.StartAgentSessionRequest{Workspace: running})
	for _, id := range []string{done, recent, running} {
		complete(id)
	}
	// A completed workspace made in Orca, outside the workspaces folder:
	// its project is removed but never its folder.
	f.fakes.Orca.AddRepo(core.Repo{ID: "elsewhere", DisplayName: "reach-elsewhere", Path: "/home/me/reach-elsewhere", Kind: "folder"})
	f.fakes.Folders.Make("/home/me/reach-elsewhere")
	f.fakes.Orca.SetWorktree(ctx, "elsewhere::/home/me/reach-elsewhere", "", "completed")
	for _, id := range []string{done, recent, running, inProgress, "elsewhere"} {
		f.fakes.Orca.Touch(id, old)
	}
	at(now)
	f.svc.ReadFile(ctx, f.a, service.ReadFileRequest{Workspace: recent, Path: "x"})

	removed, err := f.svc.Cleanup(ctx, now)
	slices.Sort(removed)
	want := []string{done, "elsewhere"}
	slices.Sort(want)
	if err != nil || !slices.Equal(removed, want) {
		t.Fatalf("removed %q %v", removed, err)
	}
	if f.fakes.Folders.Exists("/w/reach-done") || !f.fakes.Folders.Exists("/home/me/reach-elsewhere") {
		t.Fatal("folders: Reach's must go, others stay")
	}
	for _, id := range []string{recent, running, inProgress} {
		if f.fakes.Orca.Root(id) == nil || !f.fakes.Folders.Exists(f.fakes.Orca.Root(id).Path) {
			t.Errorf("workspace %s was removed", id)
		}
	}
	if _, err := f.svc.ReadFile(ctx, f.a, service.ReadFileRequest{Workspace: done, Path: "x"}); code(err) != core.CodeNotFound {
		t.Fatalf("removed workspace still usable: %v", err)
	}
	// Nothing else is due: a second run removes nothing.
	if removed, err := f.svc.Cleanup(ctx, now); err != nil || len(removed) != 0 {
		t.Fatalf("second run removed %q %v", removed, err)
	}
}

// Cleanup never removes a workspace a request is using: here it runs while
// a command runs in a workspace that is otherwise due (a command leaves no
// record that makes the workspace busy).
func TestCleanupSkipsWorkspacesInUse(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	now := time.Now()
	f.fakes.Store.Now = func() time.Time { return now.Add(-core.CleanupAfter - time.Hour) }
	f.svc.UpdateWorkspace(ctx, f.a, service.UpdateWorkspaceRequest{OperationID: "done", Workspace: f.ws, Status: "completed"})
	f.fakes.Orca.Touch(f.ws, now.Add(-core.CleanupAfter-time.Hour))
	f.fakes.Store.Now = func() time.Time { return now.Add(-core.CleanupAfter - time.Hour) }

	var during []string
	f.fakes.Commands.Result = func(core.CommandSpec) (*core.CommandResult, error) {
		var err error
		if during, err = f.svc.Cleanup(ctx, now); err != nil {
			t.Error(err)
		}
		return &core.CommandResult{}, nil
	}
	if op, err := f.svc.RunCommand(ctx, f.a, service.RunCommandRequest{OperationID: "c", Workspace: f.ws, Executable: "/bin/sh"}); err != nil || op.State != core.StateSucceeded {
		t.Fatalf("run_command: %+v %v", op, err)
	}
	if len(during) != 0 || f.fakes.Orca.Root(f.ws) == nil || !f.fakes.Folders.Exists(f.root) {
		t.Fatalf("cleanup removed a workspace while a command ran in it: %q", during)
	}
	// Once the request is over, the workspace is due again.
	if removed, err := f.svc.Cleanup(ctx, now); err != nil || len(removed) != 1 {
		t.Fatalf("after the command: %q %v", removed, err)
	}
	// And a request cannot take a workspace being removed: it is gone.
	if _, err := f.svc.ReadFile(ctx, f.a, service.ReadFileRequest{Workspace: f.ws, Path: "x"}); code(err) != core.CodeNotFound {
		t.Fatalf("removed workspace: %v", err)
	}
}
