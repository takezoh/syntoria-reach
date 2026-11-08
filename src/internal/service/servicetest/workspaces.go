package servicetest

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/takezoh/syntoria-reach/src/internal/core"
	"github.com/takezoh/syntoria-reach/src/internal/service"
)

// Backend is a workspace backend under test: it keeps its folders under Dir
// in Folders.
type Backend struct {
	Workspaces service.Workspaces
	Folders    *Folders
	Dir        string
}

// WorkspacesContract checks that a workspace backend keeps the contract the
// service relies on, whichever backend the node has. open returns a backend
// with no workspaces, whose agents start at once and reply to a prompt with
// "echo: <prompt>".
func WorkspacesContract(t *testing.T, open func(t *testing.T) Backend) {
	ctx := context.Background()
	agent := core.Agent{ID: "coder", Role: "Writes code", Command: []string{"/bin/coder"}}

	t.Run("workspaces", func(t *testing.T) {
		b := open(t)
		w, err := b.Workspaces.Create(ctx, "reach-docs", "fix the docs")
		want := core.Workspace{ID: w.ID, Name: "reach-docs", Note: "fix the docs", Status: core.WorkspaceInProgress, Path: filepath.Join(b.Dir, "reach-docs")}
		if err != nil || w.ID == "" || !same(w, want) || !b.Folders.Exists(want.Path) {
			t.Fatalf("create: %+v %v", w, err)
		}
		if got, err := b.Workspaces.Get(ctx, w.ID); err != nil || !same(got, want) {
			t.Fatalf("get: %+v %v", got, err)
		}
		// The folder of a workspace exists: the name is taken.
		if _, err := b.Workspaces.Create(ctx, "reach-docs", "again"); code(err) != core.CodeInvalidState {
			t.Fatalf("same name: %v", err)
		}
		other, _ := b.Workspaces.Create(ctx, "reach-api", "add the API")
		list, err := b.Workspaces.List(ctx)
		ids := []string{}
		for _, l := range list {
			ids = append(ids, l.ID)
		}
		if err != nil || len(list) != 2 || !slices.Contains(ids, w.ID) || !slices.Contains(ids, other.ID) {
			t.Fatalf("list: %v %v", ids, err)
		}

		u, err := b.Workspaces.Update(ctx, w.ID, "", core.WorkspaceInReview)
		if err != nil || u.Status != core.WorkspaceInReview || u.Note != "fix the docs" {
			t.Fatalf("status: %+v %v", u, err)
		}
		u, err = b.Workspaces.Update(ctx, w.ID, "docs: tests added", "")
		if err != nil || u.Status != core.WorkspaceInReview || u.Note != "docs: tests added" {
			t.Fatalf("note: %+v %v", u, err)
		}
		if got, _ := b.Workspaces.Get(ctx, w.ID); got.Note != "docs: tests added" || got.Status != core.WorkspaceInReview || got.LastActivity.IsZero() {
			t.Fatalf("after updates: %+v", got)
		}
		if _, err := b.Workspaces.Update(ctx, "ws-nope", "x", ""); !errors.Is(err, core.ErrNotFound) {
			t.Fatalf("update of an unknown workspace: %v", err)
		}
		if _, err := b.Workspaces.Get(ctx, "ws-nope"); !errors.Is(err, core.ErrNotFound) {
			t.Fatalf("unknown workspace: %v", err)
		}

		if err := b.Workspaces.Remove(ctx, u); err != nil {
			t.Fatal(err)
		}
		if _, err := b.Workspaces.Get(ctx, w.ID); !errors.Is(err, core.ErrNotFound) || b.Folders.Exists(want.Path) {
			t.Fatalf("after removal: %v, folder kept %v", err, b.Folders.Exists(want.Path))
		}
		if list, _ := b.Workspaces.List(ctx); len(list) != 1 || list[0].ID != other.ID {
			t.Fatalf("list after removal: %+v", list)
		}
	})

	t.Run("sweep", func(t *testing.T) {
		b := open(t)
		now := time.Now()
		kept, _ := b.Workspaces.Create(ctx, "reach-kept", "kept")
		for name, at := range map[string]time.Time{"reach-stray": now.Add(-time.Hour), "reach-fresh": now, "notes": now.Add(-time.Hour)} {
			b.Folders.Make(filepath.Join(b.Dir, name))
			b.Folders.Change(filepath.Join(b.Dir, name), at)
		}
		b.Folders.Change(kept.Path, now.Add(-time.Hour))
		removed, err := b.Workspaces.Sweep(ctx, now.Add(-time.Minute))
		if err != nil || !slices.Equal(removed, []string{"reach-stray"}) {
			t.Fatalf("swept %q %v", removed, err)
		}
		for _, name := range []string{"reach-kept", "reach-fresh", "notes"} {
			if !b.Folders.Exists(filepath.Join(b.Dir, name)) {
				t.Errorf("%s was removed", name)
			}
		}
	})

	t.Run("agent sessions", func(t *testing.T) {
		b := open(t)
		w, _ := b.Workspaces.Create(ctx, "reach-ws", "work")
		s, err := b.Workspaces.StartAgent(ctx, w, agent)
		if err != nil || s.ID == "" || s.Workspace != w.ID || s.Agent == "" || s.State != core.AgentIdle {
			t.Fatalf("start: %+v %v", s, err)
		}
		if busy, err := b.Workspaces.Busy(ctx); err != nil || !busy[w.ID] {
			t.Fatalf("busy with a session: %v %v", busy, err)
		}
		st, err := b.Workspaces.Session(ctx, s.ID)
		if err != nil || st.ID != s.ID || st.Workspace != w.ID || st.State != core.AgentIdle || st.Reply != "" {
			t.Fatalf("session before a prompt: %+v %v", st, err)
		}
		stages, err := b.Workspaces.Prompt(ctx, s.ID, "hello")
		if err != nil || !slices.Contains(stages, "turn_started") {
			t.Fatalf("prompt: %v %v", stages, err)
		}
		if st, err = b.Workspaces.Session(ctx, s.ID); err != nil || st.State != core.AgentIdle || st.Reply != "echo: hello" {
			t.Fatalf("reply: %+v %v", st, err)
		}
		if err := b.Workspaces.Stop(ctx, s.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := b.Workspaces.Session(ctx, s.ID); !errors.Is(err, core.ErrNotFound) {
			t.Fatalf("session after stop: %v", err)
		}
		if busy, _ := b.Workspaces.Busy(ctx); busy[w.ID] {
			t.Fatal("busy after the session stopped")
		}
		for name, err := range map[string]error{
			"session": func() error { _, err := b.Workspaces.Session(ctx, "ses-nope"); return err }(),
			"stop":    b.Workspaces.Stop(ctx, "ses-nope"),
		} {
			if !errors.Is(err, core.ErrNotFound) {
				t.Errorf("%s of an unknown session: %v", name, err)
			}
		}
	})
}

// same compares workspaces without their last activity.
func same(w *core.Workspace, want core.Workspace) bool {
	if w == nil {
		return false
	}
	c := *w
	c.LastActivity = time.Time{}
	return c == want
}

func code(err error) string {
	var e *core.Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

// RecordsContract checks that a keeper of local workspace records
// (localws.Records) keeps its contract. It runs against the SQLite store
// and against the fake.
func RecordsContract(t *testing.T, open func(t *testing.T) RecordKeeper) {
	r := open(t)
	before := time.Now().Add(-time.Second)
	w := &core.Workspace{ID: "ws_1", Name: "reach-a", Note: "a", Status: core.WorkspaceInProgress, Path: "/w/reach-a"}
	if err := r.CreateWorkspace(w); err != nil {
		t.Fatal(err)
	}
	if err := r.CreateWorkspace(&core.Workspace{ID: "ws_2", Name: "reach-a2", Note: "a", Status: core.WorkspaceInProgress, Path: "/w/reach-a"}); err == nil {
		t.Fatal("two records of one folder")
	}
	got, err := r.Workspace("ws_1")
	if err != nil || !same(got, *w) || got.LastActivity.Before(before) {
		t.Fatalf("get: %+v %v", got, err)
	}
	got.Note = "changed" // callers cannot change the stored record
	if got, _ = r.Workspace("ws_1"); got.Note != "a" {
		t.Fatalf("stored record changed: %+v", got)
	}
	if got, err = r.UpdateWorkspace("ws_1", "", core.WorkspaceCompleted); err != nil || got.Status != core.WorkspaceCompleted || got.Note != "a" {
		t.Fatalf("update status: %+v %v", got, err)
	}
	if got, err = r.UpdateWorkspace("ws_1", "b", ""); err != nil || got.Status != core.WorkspaceCompleted || got.Note != "b" {
		t.Fatalf("update note: %+v %v", got, err)
	}
	if _, err := r.UpdateWorkspace("nope", "x", ""); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("update of an unknown record: %v", err)
	}
	r.CreateWorkspace(&core.Workspace{ID: "ws_0", Name: "reach-0", Note: "z", Status: core.WorkspaceTodo, Path: "/w/reach-0"})
	if list, err := r.Workspaces(); err != nil || len(list) != 2 || list[0].ID != "ws_0" || list[1].ID != "ws_1" {
		t.Fatalf("list by name: %+v %v", list, err)
	}
	if err := r.DeleteWorkspace("ws_1"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Workspace("ws_1"); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("after delete: %v", err)
	}
}

// RecordKeeper is the interface of a keeper of local workspace records, as
// localws.Records declares it.
type RecordKeeper interface {
	CreateWorkspace(w *core.Workspace) error
	Workspaces() ([]*core.Workspace, error)
	Workspace(id string) (*core.Workspace, error)
	UpdateWorkspace(id, note, status string) (*core.Workspace, error)
	DeleteWorkspace(id string) error
}
