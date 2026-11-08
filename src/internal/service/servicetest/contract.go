package servicetest

import (
	"errors"
	"testing"

	"github.com/takezoh/syntoria-reach/src/internal/core"
	"github.com/takezoh/syntoria-reach/src/internal/service"
)

// StoreContract checks that a service.Store keeps the contract the service
// relies on. It runs against the SQLite store and against the fake, each
// case with a store returned by open.
func StoreContract(t *testing.T, open func(t *testing.T) service.Store) {
	t.Run("operations", func(t *testing.T) {
		s := open(t)
		op, created, err := s.Admit("c", "op", "run_command", "h1")
		if err != nil || !created || op.State != core.StateRunning || op.Kind != "run_command" || op.ArgsHash != "h1" {
			t.Fatalf("admit: op=%+v created=%v err=%v", op, created, err)
		}
		// A resend returns the stored record and writes nothing.
		if op, created, err = s.Admit("c", "op", "write_file", "h2"); err != nil || created || op.Kind != "run_command" || op.ArgsHash != "h1" {
			t.Fatalf("readmit: op=%+v created=%v err=%v", op, created, err)
		}
		if _, created, _ = s.Admit("other", "op", "run_command", "h1"); !created {
			t.Fatal("operation IDs must be scoped per client")
		}
		op, err = s.Finish("c", "op", core.StateSucceeded, map[string]int{"exit_code": 0}, "", "")
		if err != nil || op.State != core.StateSucceeded || string(op.Result) != `{"exit_code":0}` {
			t.Fatalf("finish: op=%+v err=%v", op, err)
		}
		// An ended operation is never overwritten.
		if op, _ = s.Finish("c", "op", core.StateFailed, nil, "internal", "late"); op.State != core.StateSucceeded || op.ErrorCode != "" {
			t.Fatalf("late finish: %+v", op)
		}
		if op, _ = s.GetOperation("c", "op"); string(op.Result) != `{"exit_code":0}` {
			t.Fatalf("get: %+v", op)
		}
		op.Result[0] = 'x' // callers cannot change the stored record
		if op, _ = s.GetOperation("c", "op"); string(op.Result) != `{"exit_code":0}` {
			t.Fatalf("stored record changed: %s", op.Result)
		}
		if _, err := s.GetOperation("c", "missing"); !errors.Is(err, core.ErrNotFound) {
			t.Fatalf("missing: %v", err)
		}
		if op, _ = s.Finish("other", "op", core.StateFailed, nil, "orca_failed", "boom"); op.ErrorCode != "orca_failed" || op.ErrorMessage != "boom" || op.Result != nil {
			t.Fatalf("failed finish: %+v", op)
		}
	})

	t.Run("workspaces", func(t *testing.T) {
		s := open(t)
		for _, id := range []string{"ws_2", "ws_1"} {
			if err := s.CreateWorkspace(&core.Workspace{ID: id, ClientID: "c", ProjectID: "p", Name: id}); err != nil {
				t.Fatal(err)
			}
		}
		if err := s.CreateWorkspace(&core.Workspace{ID: "ws_1", ClientID: "c"}); err == nil {
			t.Fatal("duplicate workspace ID accepted")
		}
		w, err := s.GetWorkspace("c", "ws_1")
		if err != nil || w.State != core.WorkspaceCreating || w.ProjectID != "p" || w.Name != "ws_1" {
			t.Fatalf("get: %+v %v", w, err)
		}
		if _, err := s.GetWorkspace("other", "ws_1"); !errors.Is(err, core.ErrNotFound) {
			t.Fatalf("other client: %v", err)
		}
		if list, _ := s.ListWorkspaces("c"); len(list) != 0 {
			t.Fatalf("creating workspaces listed: %+v", list)
		}
		if ok, err := s.SetWorkspaceState("ws_1", core.WorkspaceReady, core.WorkspaceDeleting, "", "", "", ""); ok || err != nil {
			t.Fatalf("move from a wrong state: ok=%v err=%v", ok, err)
		}
		for _, id := range []string{"ws_1", "ws_2"} {
			if ok, err := s.SetWorkspaceState(id, core.WorkspaceCreating, core.WorkspaceReady, "orca-"+id, "/w/"+id, "b", ""); !ok || err != nil {
				t.Fatalf("ready: ok=%v err=%v", ok, err)
			}
		}
		// Empty orca ID, path and branch are kept; the message is replaced.
		s.SetWorkspaceState("ws_1", core.WorkspaceReady, core.WorkspaceDeleting, "", "", "", "")
		s.SetWorkspaceState("ws_1", core.WorkspaceDeleting, core.WorkspaceReady, "", "", "", "dirty")
		w, _ = s.GetWorkspace("c", "ws_1")
		if w.State != core.WorkspaceReady || w.OrcaID != "orca-ws_1" || w.Path != "/w/ws_1" || w.Branch != "b" || w.ErrorMessage != "dirty" {
			t.Fatalf("after failed delete: %+v", w)
		}
		// The order of the list is not part of the contract.
		list, err := s.ListWorkspaces("c")
		if err != nil || len(list) != 2 || list[0].ID == list[1].ID ||
			(list[0].ID != "ws_1" && list[0].ID != "ws_2") || (list[1].ID != "ws_1" && list[1].ID != "ws_2") {
			t.Fatalf("list: %+v %v", list, err)
		}
		if list, _ := s.ListWorkspaces("other"); len(list) != 0 {
			t.Fatalf("other client lists: %+v", list)
		}
		if busy, err := s.WorkspaceBusy("ws_1"); busy || err != nil {
			t.Fatalf("idle workspace busy=%v err=%v", busy, err)
		}
	})

	t.Run("jobs", func(t *testing.T) {
		s := open(t)
		if err := s.CreateJob(&core.Job{ID: "job_1", ClientID: "c", WorkspaceID: "ws", Executable: "/bin/sh"}); err != nil {
			t.Fatal(err)
		}
		j, err := s.GetJob("c", "job_1")
		if err != nil || j.State != core.JobRunning || j.WorkspaceID != "ws" || j.Executable != "/bin/sh" || j.ExitCode != nil {
			t.Fatalf("get: %+v %v", j, err)
		}
		if busy, _ := s.WorkspaceBusy("ws"); !busy {
			t.Fatal("running job does not make the workspace busy")
		}
		code := 2
		if err := s.FinishJob("job_1", core.JobFailed, &code, true, true, "timed out"); err != nil {
			t.Fatal(err)
		}
		code = 9 // the store keeps its own copy
		if err := s.FinishJob("job_1", core.JobSucceeded, nil, false, false, ""); err != nil {
			t.Fatal(err)
		}
		j, _ = s.GetJob("c", "job_1")
		if j.State != core.JobFailed || j.ExitCode == nil || *j.ExitCode != 2 || !j.TimedOut || !j.Truncated || j.ErrorMessage != "timed out" {
			t.Fatalf("finished job: %+v", j)
		}
		if busy, _ := s.WorkspaceBusy("ws"); busy {
			t.Fatal("ended job keeps the workspace busy")
		}
		if _, err := s.GetJob("other", "job_1"); !errors.Is(err, core.ErrNotFound) {
			t.Fatalf("other client: %v", err)
		}
		if err := s.FinishJob("missing", core.JobFailed, nil, false, false, ""); err != nil {
			t.Fatalf("finish of an unknown job: %v", err)
		}
	})

	t.Run("agent sessions", func(t *testing.T) {
		s := open(t)
		if err := s.CreateAgentSession(&core.AgentSession{ID: "as_1", ClientID: "c", WorkspaceID: "ws", AgentID: "codex"}); err != nil {
			t.Fatal(err)
		}
		a, err := s.GetAgentSession("c", "as_1")
		if err != nil || a.State != core.SessionStarting || a.AgentID != "codex" || a.Turns != 0 {
			t.Fatalf("get: %+v %v", a, err)
		}
		if busy, _ := s.WorkspaceBusy("ws"); !busy {
			t.Fatal("active session does not make the workspace busy")
		}
		s.UpdateAgentSession("as_1", core.AgentSessionUpdate{State: core.SessionIdle, ProtocolSessionID: "thread"})
		s.UpdateAgentSession("as_1", core.AgentSessionUpdate{State: core.SessionRunning, AddTurn: true})
		s.UpdateAgentSession("as_1", core.AgentSessionUpdate{ErrorMessage: "note"})
		a, _ = s.GetAgentSession("c", "as_1")
		if a.State != core.SessionRunning || a.ProtocolSessionID != "thread" || a.Turns != 1 || a.ErrorMessage != "note" {
			t.Fatalf("updated: %+v", a)
		}
		// An ended session is never changed.
		s.UpdateAgentSession("as_1", core.AgentSessionUpdate{State: core.SessionFailed, ErrorMessage: "harness exited"})
		if err := s.UpdateAgentSession("as_1", core.AgentSessionUpdate{State: core.SessionIdle, AddTurn: true}); err != nil {
			t.Fatal(err)
		}
		a, _ = s.GetAgentSession("c", "as_1")
		if a.State != core.SessionFailed || a.Turns != 1 || a.ErrorMessage != "harness exited" {
			t.Fatalf("ended session changed: %+v", a)
		}
		if busy, _ := s.WorkspaceBusy("ws"); busy {
			t.Fatal("ended session keeps the workspace busy")
		}
		if _, err := s.GetAgentSession("other", "as_1"); !errors.Is(err, core.ErrNotFound) {
			t.Fatalf("other client: %v", err)
		}
		if err := s.UpdateAgentSession("missing", core.AgentSessionUpdate{State: core.SessionIdle}); err != nil {
			t.Fatalf("update of an unknown session: %v", err)
		}
	})

	t.Run("agent events", func(t *testing.T) {
		s := open(t)
		if events, err := s.ListAgentEvents("as_1", 0, 10); err != nil || events == nil || len(events) != 0 {
			t.Fatalf("no events: %#v %v", events, err)
		}
		for i, text := range []string{"a", "b", "c"} {
			if err := s.AppendAgentEvent("as_1", i/2+1, core.EventMessage, text); err != nil {
				t.Fatal(err)
			}
		}
		s.AppendAgentEvent("as_2", 1, core.EventError, "other session")
		events, _ := s.ListAgentEvents("as_1", 1, 1)
		if len(events) != 1 || events[0].Seq != 2 || events[0].Text != "b" || events[0].Turn != 1 || events[0].Type != core.EventMessage {
			t.Fatalf("page: %+v", events)
		}
		events, _ = s.ListAgentEvents("as_1", 0, 10)
		if len(events) != 3 || events[2].Seq != 3 || events[2].Turn != 2 {
			t.Fatalf("all: %+v", events)
		}
		if events, _ = s.ListAgentEvents("as_2", 0, 10); len(events) != 1 || events[0].Seq != 1 {
			t.Fatalf("sequences are per session: %+v", events)
		}
	})
}

// FilesContract checks that a service.Files keeps the contract the service
// relies on. open returns an empty workspace.
func FilesContract(t *testing.T, open func(t *testing.T) service.Files) {
	t.Run("conditional write", func(t *testing.T) {
		f := open(t)
		v1, err := f.Write("dir/a.txt", []byte("one"), "")
		if err != nil || v1 != core.FileVersion([]byte("one")) {
			t.Fatalf("create: %q %v", v1, err)
		}
		if _, err := f.Write("dir/a.txt", []byte("two"), ""); !errors.Is(err, core.ErrVersionConflict) {
			t.Fatalf("create over an existing file: %v", err)
		}
		if _, err := f.Write("dir/a.txt", []byte("two"), "sha256:stale"); !errors.Is(err, core.ErrVersionConflict) {
			t.Fatalf("stale version: %v", err)
		}
		if _, err := f.Write("b.txt", []byte("x"), v1); !errors.Is(err, core.ErrVersionConflict) {
			t.Fatalf("version for a missing file: %v", err)
		}
		if _, err := f.Write("dir/a.txt", []byte("two"), v1); err != nil {
			t.Fatal(err)
		}
		c, err := f.Read("dir/a.txt", 1, 10)
		if err != nil || string(c.Data) != "wo" || c.Size != 3 || c.Offset != 1 || !c.EOF || c.Version != core.FileVersion([]byte("two")) {
			t.Fatalf("read: %+v %v", c, err)
		}
		if c, _ = f.Read("dir/a.txt", 0, 1); string(c.Data) != "t" || c.EOF {
			t.Fatalf("bounded read: %+v", c)
		}
	})

	t.Run("listing and errors", func(t *testing.T) {
		f := open(t)
		f.Write("b.txt", []byte("bb"), "")
		f.Write("a/c.txt", []byte("c"), "")
		entries, truncated, err := f.List(".", 10)
		byName := map[string]core.FileEntry{} // the order is not part of the contract
		for _, e := range entries {
			byName[e.Name] = e
		}
		if err != nil || truncated || len(entries) != 2 || byName["a"].Type != "dir" ||
			byName["b.txt"].Type != "file" || byName["b.txt"].Size != 2 {
			t.Fatalf("list: %+v %v %v", entries, truncated, err)
		}
		if entries, truncated, _ = f.List("", 1); len(entries) != 1 || !truncated {
			t.Fatalf("bounded list: %+v %v", entries, truncated)
		}
		if _, err := f.Read("missing", 0, 1); !errors.Is(err, core.ErrNotFound) {
			t.Fatalf("missing file: %v", err)
		}
		if _, _, err := f.List("missing", 1); !errors.Is(err, core.ErrNotFound) {
			t.Fatalf("missing dir: %v", err)
		}
		if _, err := f.Read("a", 0, 1); !errors.Is(err, core.ErrNotRegular) {
			t.Fatalf("read of a directory: %v", err)
		}
		for _, p := range []string{"../x", "/etc/passwd", "a/../../x", "a\x00b"} {
			if _, err := f.Read(p, 0, 1); !errors.Is(err, core.ErrInvalidPath) {
				t.Errorf("read %q: %v", p, err)
			}
			if _, err := f.Write(p, nil, ""); !errors.Is(err, core.ErrInvalidPath) {
				t.Errorf("write %q: %v", p, err)
			}
		}
	})
}
