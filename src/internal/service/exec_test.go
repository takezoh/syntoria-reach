package service_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/takezoh/syntoria-reach/src/internal/core"
	"github.com/takezoh/syntoria-reach/src/internal/service"
)

func (f *fixture) startJob(t *testing.T, opID string) *core.Job {
	t.Helper()
	op, err := f.svc.StartJob(f.a, service.StartJobRequest{OperationID: opID, Workspace: f.ws, Executable: "/bin/sh", Args: []string{"-c", "x"}})
	if err != nil || op.State != core.StateSucceeded {
		t.Fatalf("start job: %+v %v", op, err)
	}
	j := result[core.Job](t, op)
	return &j
}

func (f *fixture) getJob(t *testing.T, id string) *core.Job {
	t.Helper()
	j, err := f.svc.GetJob(f.a, service.JobRequest{JobID: id})
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func TestStartJob(t *testing.T) {
	f := newFixture(t)
	if _, err := f.svc.StartJob(f.b, service.StartJobRequest{OperationID: "j", Workspace: f.ws, Executable: "/bin/sh"}); code(err) != core.CodePermissionDenied {
		t.Fatalf("client without command.run: %v", err)
	}
	if _, err := f.svc.StartJob(f.a, service.StartJobRequest{OperationID: "j", Workspace: f.ws, Executable: "/bin/bash"}); code(err) != core.CodePermissionDenied {
		t.Fatalf("executable not in allowlist: %v", err)
	}
	if _, err := f.svc.StartJob(f.a, service.StartJobRequest{OperationID: "j", Workspace: f.ws, Executable: "/bin/sh", TimeoutSeconds: 24*3600 + 1}); code(err) != core.CodeInvalidRequest {
		t.Fatalf("timeout out of range: %v", err)
	}

	j := f.startJob(t, "j1")
	if j.State != core.JobRunning || j.WorkspaceID != f.ws || j.Executable != "/bin/sh" {
		t.Fatalf("job = %+v", j)
	}
	spec, ok := f.fakes.Jobs.Spec(j.ID)
	if !ok || spec.Dir != f.root || spec.Timeout != time.Hour || strings.Join(spec.Args, " ") != "-c x" {
		t.Fatalf("spec = %+v", spec)
	}
	if again := f.startJob(t, "j1"); again.ID != j.ID {
		t.Fatal("resend started another job")
	}
	if _, err := f.svc.GetJob(f.b, service.JobRequest{JobID: j.ID}); code(err) != core.CodeNotFound {
		t.Fatalf("other client sees job: %v", err)
	}
}

func TestJobOutcomeIsRecorded(t *testing.T) {
	zero, two := 0, 2
	cases := map[string]struct {
		exit  core.JobExit
		state string
		msg   string
	}{
		"success":  {core.JobExit{ExitCode: &zero, Truncated: true}, core.JobSucceeded, ""},
		"failure":  {core.JobExit{ExitCode: &two}, core.JobFailed, ""},
		"timeout":  {core.JobExit{TimedOut: true}, core.JobFailed, "timed out"},
		"signaled": {core.JobExit{}, core.JobFailed, "terminated by a signal"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			j := f.startJob(t, "j")
			if err := f.fakes.Jobs.Exit(j.ID, c.exit); err != nil {
				t.Fatal(err)
			}
			got := f.getJob(t, j.ID)
			if got.State != c.state || got.ErrorMessage != c.msg || got.TimedOut != c.exit.TimedOut || got.Truncated != c.exit.Truncated ||
				(c.exit.ExitCode == nil) != (got.ExitCode == nil) {
				t.Fatalf("job = %+v", got)
			}
		})
	}
}

func TestJobStartFailure(t *testing.T) {
	f := newFixture(t)
	f.fakes.Jobs.StartErr = errors.New("start: no such file")
	j := f.startJob(t, "j")
	if j.State != core.JobFailed || j.ErrorMessage != "start: no such file" {
		t.Fatalf("job = %+v", j)
	}
	// A job that never ran does not keep the workspace busy.
	op, err := f.svc.DeleteWorkspace(context.Background(), f.a, service.WorkspaceRequest{OperationID: "d", Workspace: f.ws})
	if err != nil || op.State != core.StateSucceeded {
		t.Fatalf("delete: %+v %v", op, err)
	}
}

func TestCancelAndReadJobOutput(t *testing.T) {
	f := newFixture(t)
	j := f.startJob(t, "j")
	f.fakes.Jobs.SetOutput(j.ID, "stdout", []byte("started\n"))

	// While the job runs, reaching the end of the output is not EOF.
	out, err := f.svc.ReadJobOutput(f.a, service.ReadJobOutputRequest{JobID: j.ID, Stream: "stdout"})
	if err != nil || string(out.Data) != "started\n" || out.EOF || out.State != core.JobRunning {
		t.Fatalf("output = %+v %v", out, err)
	}
	if _, err := f.svc.ReadJobOutput(f.a, service.ReadJobOutputRequest{JobID: j.ID, Stream: "log"}); code(err) != core.CodeInvalidRequest {
		t.Fatalf("unknown stream: %v", err)
	}
	if _, err := f.svc.CancelJob(f.b, service.CancelJobRequest{OperationID: "c", JobID: j.ID}); code(err) != core.CodeNotFound {
		t.Fatalf("other client cancels: %v", err)
	}

	op, err := f.svc.CancelJob(f.a, service.CancelJobRequest{OperationID: "c1", JobID: j.ID})
	if err != nil || result[core.Job](t, op).State != core.JobCancelled {
		t.Fatalf("cancel: %+v %v", op, err)
	}
	out, _ = f.svc.ReadJobOutput(f.a, service.ReadJobOutputRequest{JobID: j.ID, Stream: "stdout", Offset: 4})
	if string(out.Data) != "ted\n" || !out.EOF || out.State != core.JobCancelled {
		t.Fatalf("output after cancel = %+v", out)
	}
	// Cancelling an ended job succeeds and leaves it unchanged.
	if op, err = f.svc.CancelJob(f.a, service.CancelJobRequest{OperationID: "c2", JobID: j.ID}); err != nil || op.State != core.StateSucceeded || result[core.Job](t, op).State != core.JobCancelled {
		t.Fatalf("second cancel: %+v %v", op, err)
	}
	// A late exit report does not overwrite the outcome.
	f.fakes.Jobs.Exit(j.ID, core.JobExit{})
	if got := f.getJob(t, j.ID); got.State != core.JobCancelled {
		t.Fatalf("job = %+v", got)
	}
}

func (f *fixture) startSession(t *testing.T, opID string, req service.StartAgentSessionRequest) *core.AgentSession {
	t.Helper()
	req.OperationID = opID
	if req.Workspace == "" {
		req.Workspace = f.ws
	}
	if req.Agent == "" {
		req.Agent = "codex"
	}
	op, err := f.svc.StartAgentSession(context.Background(), f.a, req)
	if err != nil || op.State != core.StateSucceeded {
		t.Fatalf("start session: %+v %v", op, err)
	}
	s := result[core.AgentSession](t, op)
	return &s
}

func (f *fixture) session(t *testing.T, id string) (*core.AgentSession, string) {
	t.Helper()
	events, err := f.svc.ReadAgentEvents(f.a, service.ReadAgentEventsRequest{SessionID: id})
	if err != nil {
		t.Fatal(err)
	}
	var parts []string
	for _, e := range events.Events {
		parts = append(parts, e.Type+":"+e.Text)
	}
	s, err := f.svc.GetAgentSession(f.a, service.AgentSessionRequest{SessionID: id})
	if err != nil {
		t.Fatal(err)
	}
	return s, strings.Join(parts, " | ")
}

func (f *fixture) prompt(t *testing.T, opID, sessionID, text string) *core.Operation {
	t.Helper()
	op, err := f.svc.SendAgentPrompt(f.a, service.SendAgentPromptRequest{OperationID: opID, SessionID: sessionID, Text: text})
	if err != nil {
		t.Fatal(err)
	}
	return op
}

func TestStartAgentSession(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	denied := map[string]service.StartAgentSessionRequest{
		"client without agent.run": {OperationID: "s", Workspace: f.ws, Agent: "codex"},
		"unknown agent":            {OperationID: "s", Workspace: f.ws, Agent: "other"},
	}
	for name, req := range denied {
		client := f.a
		if name == "client without agent.run" {
			client = f.b
		}
		if _, err := f.svc.StartAgentSession(ctx, client, req); code(err) != core.CodePermissionDenied {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := f.svc.StartAgentSession(ctx, f.a, service.StartAgentSessionRequest{OperationID: "s", Workspace: f.ws, Agent: "dsh", Model: "m"}); code(err) != core.CodeInvalidRequest {
		t.Errorf("model for an ACP agent: %v", err)
	}

	s := f.startSession(t, "s1", service.StartAgentSessionRequest{Model: "gpt", Effort: "high"})
	if s.State != core.SessionIdle || s.ProtocolSessionID != "proto-"+s.ID || s.AgentID != "codex" || s.WorkspaceID != f.ws {
		t.Fatalf("session = %+v", s)
	}
	start, _ := f.fakes.Agents.Started(s.ID)
	if start.Dir != f.root || start.Harness.Protocol != core.ProtocolCodex || start.Harness.Command[0] != "/bin/codex" ||
		start.Harness.Env["K"] != "V" || start.Harness.Sandbox != "workspace-write" || start.Resume != "" || start.Model != "gpt" || start.Effort != "high" {
		t.Fatalf("start = %+v", start)
	}
	if again := f.startSession(t, "s1", service.StartAgentSessionRequest{Model: "gpt", Effort: "high"}); again.ID != s.ID {
		t.Fatal("resend started another session")
	}
	if _, err := f.svc.GetAgentSession(f.b, service.AgentSessionRequest{SessionID: s.ID}); code(err) != core.CodeNotFound {
		t.Fatalf("other client sees session: %v", err)
	}

	// A harness that cannot start leaves a failed session.
	f.fakes.Agents.StartErr = errors.New("open session: timeout")
	op, err := f.svc.StartAgentSession(ctx, f.a, service.StartAgentSessionRequest{OperationID: "s2", Workspace: f.ws, Agent: "codex"})
	if err != nil || op.State != core.StateFailed || op.ErrorCode != core.CodeHarnessFailed {
		t.Fatalf("failed start: %+v %v", op, err)
	}
	if failed, _ := f.fakes.Store.GetAgentSession("a", "as_3"); failed == nil || failed.State != core.SessionFailed || failed.ErrorMessage != "open session: timeout" {
		t.Fatalf("failed session = %+v", failed)
	}
}

func TestAgentTurns(t *testing.T) {
	f := newFixture(t)
	s := f.startSession(t, "s", service.StartAgentSessionRequest{})
	if _, err := f.svc.SendAgentPrompt(f.a, service.SendAgentPromptRequest{OperationID: "p", SessionID: s.ID}); code(err) != core.CodeInvalidRequest {
		t.Fatalf("empty prompt: %v", err)
	}
	if _, err := f.svc.SendAgentPrompt(f.b, service.SendAgentPromptRequest{OperationID: "p", SessionID: s.ID, Text: "hi"}); code(err) != core.CodeNotFound {
		t.Fatalf("other client prompts session: %v", err)
	}

	if op := f.prompt(t, "p1", s.ID, "hi"); string(op.Result) != `{"turn":1}` {
		t.Fatalf("prompt: %+v", op)
	}
	if got, _ := f.session(t, s.ID); got.State != core.SessionRunning || got.Turns != 1 {
		t.Fatalf("during the turn: %+v", got)
	}
	// One turn at a time; resending the prompt does not start a second one.
	if op := f.prompt(t, "p2", s.ID, "again"); op.State != core.StateFailed || op.ErrorCode != core.CodeInvalidState {
		t.Fatalf("concurrent prompt: %+v", op)
	}
	if op := f.prompt(t, "p1", s.ID, "hi"); string(op.Result) != `{"turn":1}` {
		t.Fatalf("resend: %+v", op)
	}
	f.fakes.Agents.Emit(s.ID, core.EventMessage, "hello")
	f.fakes.Agents.EndTurn(s.ID, "completed")
	got, events := f.session(t, s.ID)
	if got.State != core.SessionIdle || got.Turns != 1 || events != "message:hello | turn_completed:completed" {
		t.Fatalf("after the turn: %+v %s", got, events)
	}
	page, _ := f.svc.ReadAgentEvents(f.a, service.ReadAgentEventsRequest{SessionID: s.ID, After: 1, Limit: 1})
	if len(page.Events) != 1 || page.Events[0].Seq != 2 || page.Events[0].Turn != 1 {
		t.Fatalf("page = %+v", page)
	}
	if _, err := f.svc.ReadAgentEvents(f.a, service.ReadAgentEventsRequest{SessionID: s.ID, Limit: 1001}); code(err) != core.CodeInvalidRequest {
		t.Fatalf("event limit: %v", err)
	}

	if op := f.prompt(t, "p3", s.ID, "next"); string(op.Result) != `{"turn":2}` {
		t.Fatalf("second turn: %+v", op)
	}
	stop, err := f.svc.StopAgentSession(f.a, service.StopAgentSessionRequest{OperationID: "x1", SessionID: s.ID})
	if err != nil || result[core.AgentSession](t, stop).State != core.SessionClosed {
		t.Fatalf("stop: %+v %v", stop, err)
	}
	if _, events = f.session(t, s.ID); !strings.HasSuffix(events, "turn_completed:cancelled") {
		t.Fatalf("interrupted turn: %s", events)
	}
	if op := f.prompt(t, "p4", s.ID, "late"); op.State != core.StateFailed || op.ErrorCode != core.CodeInvalidState {
		t.Fatalf("prompt after stop: %+v", op)
	}
	// Stopping an ended session succeeds and leaves it unchanged.
	if stop, err = f.svc.StopAgentSession(f.a, service.StopAgentSessionRequest{OperationID: "x2", SessionID: s.ID}); err != nil || result[core.AgentSession](t, stop).State != core.SessionClosed {
		t.Fatalf("second stop: %+v %v", stop, err)
	}
}

func TestHarnessExitFailsSession(t *testing.T) {
	f := newFixture(t)
	s := f.startSession(t, "s", service.StartAgentSessionRequest{})
	f.prompt(t, "p", s.ID, "crash")
	f.fakes.Agents.Exit(s.ID)
	got, events := f.session(t, s.ID)
	if got.State != core.SessionFailed || got.ErrorMessage != "harness exited" || events != "error:harness stream ended | error:harness exited" {
		t.Fatalf("session = %+v events = %s", got, events)
	}
	// The failed session no longer blocks deleting the workspace, and a stop
	// leaves it failed.
	if stop, err := f.svc.StopAgentSession(f.a, service.StopAgentSessionRequest{OperationID: "x", SessionID: s.ID}); err != nil || result[core.AgentSession](t, stop).State != core.SessionFailed {
		t.Fatalf("stop: %+v %v", stop, err)
	}
}

func TestResumeAgentSession(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	s := f.startSession(t, "s1", service.StartAgentSessionRequest{})
	resume := service.StartAgentSessionRequest{ResumeSessionID: s.ID}
	if _, err := f.svc.StartAgentSession(ctx, f.a, service.StartAgentSessionRequest{OperationID: "r", Workspace: f.ws, Agent: "codex", ResumeSessionID: s.ID}); code(err) != core.CodeInvalidState {
		t.Fatalf("resume of an active session: %v", err)
	}
	f.svc.StopAgentSession(f.a, service.StopAgentSessionRequest{OperationID: "x", SessionID: s.ID})

	// Only the owner can resume, in the same workspace and agent.
	other := service.StartAgentSessionRequest{OperationID: "r", Workspace: f.newWorkspace(t, f.c, "c-ws"), Agent: "codex", ResumeSessionID: s.ID}
	if _, err := f.svc.StartAgentSession(ctx, f.c, other); code(err) != core.CodeNotFound {
		t.Fatalf("other client resumes session: %v", err)
	}
	if _, err := f.svc.StartAgentSession(ctx, f.a, service.StartAgentSessionRequest{OperationID: "r", Workspace: f.ws, Agent: "dsh", ResumeSessionID: s.ID}); code(err) != core.CodeInvalidRequest {
		t.Fatalf("resume with another agent: %v", err)
	}
	resumed := f.startSession(t, "s2", resume)
	start, _ := f.fakes.Agents.Started(resumed.ID)
	if resumed.ID == s.ID || start.Resume != s.ProtocolSessionID || resumed.ProtocolSessionID != s.ProtocolSessionID {
		t.Fatalf("resumed = %+v start = %+v previous = %+v", resumed, start, s)
	}
}
