package localws_test

import (
	"context"
	"errors"
	"testing"

	"github.com/takezoh/syntoria-reach/src/internal/core"
	"github.com/takezoh/syntoria-reach/src/internal/localws"
	"github.com/takezoh/syntoria-reach/src/internal/service/servicetest"
)

func newBackend(f *servicetest.Fakes) *localws.Workspaces {
	return localws.New("/w", f.Records, f.Folders, f.Agents, servicetest.NewIDs())
}

func TestWorkspacesContract(t *testing.T) {
	servicetest.WorkspacesContract(t, func(t *testing.T) servicetest.Backend {
		f := servicetest.New()
		return servicetest.Backend{Workspaces: newBackend(f), Folders: f.Folders, Dir: "/w"}
	})
}

func TestLocalWorkspaces(t *testing.T) {
	ctx := context.Background()
	f := servicetest.New()
	b := newBackend(f)
	w, err := b.Create(ctx, "reach-ws", "work")
	if err != nil {
		t.Fatal(err)
	}
	// A local workspace knows the statuses every backend knows, no others.
	if _, err := b.Update(ctx, w.ID, "", "archived"); code(err) != core.CodeInvalidRequest {
		t.Fatalf("unknown status: %v", err)
	}
	// A workspace that cannot be recorded leaves no folder behind.
	f.Records.FailCreate = true
	if _, err := b.Create(ctx, "reach-lost", "x"); err == nil || f.Folders.Exists("/w/reach-lost") {
		t.Fatalf("unrecorded workspace: %v, folder %v", err, f.Folders.Exists("/w/reach-lost"))
	}
	f.Records.FailCreate = false
}

func TestLocalSessions(t *testing.T) {
	ctx := context.Background()
	f := servicetest.New()
	b := newBackend(f)
	w, _ := b.Create(ctx, "reach-ws", "work")
	agent := core.Agent{ID: "claude", Role: "Writes code", Command: []string{"/bin/claude"}, Protocol: core.ProtocolClaude}

	// The agent runs in the workspace folder, as configured.
	s, err := b.StartAgent(ctx, w, agent)
	if _, dir := f.Agents.Prompts(s.ID); err != nil || dir != w.Path || s.Agent != "claude" || f.Agents.Started()[0].Protocol != core.ProtocolClaude {
		t.Fatalf("start: %+v %v in %q", s, err, dir)
	}
	// An agent that does not start leaves no session.
	f.Agents.FailStart = true
	if _, err := b.StartAgent(ctx, w, agent); code(err) != core.CodeHarnessFailed {
		t.Fatalf("failed start: %v", err)
	}
	f.Agents.FailStart = false
	if busy, _ := b.Busy(ctx); len(busy) != 1 || f.Agents.Running() != 1 {
		t.Fatalf("after a failed start: busy %v, %d agents", busy, f.Agents.Running())
	}

	// An agent that quit is exited until its session is stopped, and still
	// keeps its workspace busy.
	f.Agents.SetState(s.ID, core.AgentExited)
	if st, err := b.Session(ctx, s.ID); err != nil || st.State != core.AgentExited {
		t.Fatalf("exited agent: %+v %v", st, err)
	}
	if busy, _ := b.Busy(ctx); !busy[w.ID] {
		t.Fatal("an exited session that was not stopped does not keep its workspace")
	}
	if _, err := b.Prompt(ctx, "ses-nope", "x"); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("prompt to an unknown session: %v", err)
	}
	if err := b.Stop(ctx, s.ID); err != nil || f.Agents.Running() != 0 {
		t.Fatalf("stop: %v, %d agents", err, f.Agents.Running())
	}
}

func code(err error) string {
	var e *core.Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}
