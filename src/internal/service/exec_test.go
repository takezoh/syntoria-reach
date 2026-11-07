package service

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/takezoh/syntoria-reach/src/internal/agent/agenttest"
	"github.com/takezoh/syntoria-reach/src/internal/config"
	"github.com/takezoh/syntoria-reach/src/internal/orca/orcatest"
	"github.com/takezoh/syntoria-reach/src/internal/store"
)

// TestFakeHarness is not a test: it turns the test binary into a fake agent
// harness or a fake Orca CLI.
func TestFakeHarness(t *testing.T) {
	orcatest.MaybeRun()
	agenttest.MaybeRun()
	t.Skip("helper process")
}

func TestJobs(t *testing.T) {
	f := newFixture(t)
	if _, err := f.svc.StartJob(f.b, StartJobRequest{OperationID: "j", Workspace: f.ws, Executable: "/bin/sh"}); code(err) != CodePermissionDenied {
		t.Fatalf("client without command.run: %v", err)
	}
	if _, err := f.svc.StartJob(f.a, StartJobRequest{OperationID: "j", Workspace: f.ws, Executable: "/bin/bash"}); code(err) != CodePermissionDenied {
		t.Fatalf("executable not in allowlist: %v", err)
	}
	req := StartJobRequest{OperationID: "j1", Workspace: f.ws, Executable: "/bin/sh", Args: []string{"-c", "echo started; sleep 30"}}
	op, err := f.svc.StartJob(f.a, req)
	if err != nil || op.State != store.StateSucceeded {
		t.Fatalf("op=%+v err=%v", op, err)
	}
	var j store.Job
	json.Unmarshal(op.Result, &j)
	if again, _ := f.svc.StartJob(f.a, req); string(again.Result) != string(op.Result) {
		t.Fatal("resend started another job")
	}
	if _, err := f.svc.GetJob(f.b, JobRequest{JobID: j.ID}); code(err) != CodeNotFound {
		t.Fatalf("other client sees job: %v", err)
	}

	cancel, err := f.svc.CancelJob(f.a, CancelJobRequest{OperationID: "c1", JobID: j.ID})
	if err != nil {
		t.Fatal(err)
	}
	json.Unmarshal(cancel.Result, &j)
	if j.State != store.JobCancelled {
		t.Fatalf("after cancel: %+v", j)
	}
	out, err := f.svc.ReadJobOutput(f.a, ReadJobOutputRequest{JobID: j.ID, Stream: "stdout"})
	if err != nil || string(out.Data) != "started\n" || !out.EOF {
		t.Fatalf("output = %+v %v", out, err)
	}
}

func TestAgentSessions(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if _, err := f.svc.StartAgentSession(ctx, f.b, StartAgentSessionRequest{OperationID: "s", Workspace: f.ws, Agent: "codex"}); code(err) != CodePermissionDenied {
		t.Fatalf("client without agent.run: %v", err)
	}
	if _, err := f.svc.StartAgentSession(ctx, f.a, StartAgentSessionRequest{OperationID: "s", Workspace: f.ws, Agent: "other"}); code(err) != CodePermissionDenied {
		t.Fatalf("unknown agent: %v", err)
	}
	op, err := f.svc.StartAgentSession(ctx, f.a, StartAgentSessionRequest{OperationID: "s1", Workspace: f.ws, Agent: "codex"})
	if err != nil || op.State != store.StateSucceeded {
		t.Fatalf("op=%+v err=%v", op, err)
	}
	var sess store.AgentSession
	json.Unmarshal(op.Result, &sess)
	t.Cleanup(func() {
		f.svc.StopAgentSession(f.a, StopAgentSessionRequest{OperationID: "cleanup", SessionID: sess.ID})
	})

	prompt := SendAgentPromptRequest{OperationID: "p1", SessionID: sess.ID, Text: "hi"}
	if _, err := f.svc.SendAgentPrompt(f.b, prompt); code(err) != CodeNotFound {
		t.Fatalf("other client prompts session: %v", err)
	}
	if op, err = f.svc.SendAgentPrompt(f.a, prompt); err != nil || string(op.Result) != `{"turn":1}` {
		t.Fatalf("prompt: %+v %v", op, err)
	}
	var events *AgentEvents
	for range 200 {
		if events, err = f.svc.ReadAgentEvents(f.a, ReadAgentEventsRequest{SessionID: sess.ID}); err != nil {
			t.Fatal(err)
		}
		if events.State == store.SessionIdle {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(events.Events) != 2 || events.Events[1].Type != "turn_completed" {
		t.Fatalf("events = %+v", events)
	}
	// Resending the prompt does not start a second turn.
	if op, _ = f.svc.SendAgentPrompt(f.a, prompt); string(op.Result) != `{"turn":1}` {
		t.Fatalf("resend: %+v", op)
	}
	if got, _ := f.svc.GetAgentSession(f.a, AgentSessionRequest{SessionID: sess.ID}); got.Turns != 1 {
		t.Fatalf("turns = %d", got.Turns)
	}

	stop, err := f.svc.StopAgentSession(f.a, StopAgentSessionRequest{OperationID: "x1", SessionID: sess.ID})
	if err != nil {
		t.Fatal(err)
	}
	json.Unmarshal(stop.Result, &sess)
	if sess.State != store.SessionClosed {
		t.Fatalf("after stop: %+v", sess)
	}
	op, err = f.svc.SendAgentPrompt(f.a, SendAgentPromptRequest{OperationID: "p2", SessionID: sess.ID, Text: "late"})
	if err != nil || op.State != store.StateFailed || op.ErrorCode != CodeInvalidState {
		t.Fatalf("prompt after stop: %+v %v", op, err)
	}

	// Only the owner can resume an ended session, in the same workspace and agent.
	resume := StartAgentSessionRequest{OperationID: "s2", Workspace: f.ws, Agent: "codex", ResumeSessionID: sess.ID}
	f.b.perms["proj"] = append(f.b.perms["proj"], config.PermAgentRun)
	other := resume
	other.Workspace = f.newWorkspace(t, f.b, "b-ws")
	if _, err := f.svc.StartAgentSession(ctx, f.b, other); code(err) != CodeNotFound {
		t.Fatalf("other client resumes session: %v", err)
	}
	op, err = f.svc.StartAgentSession(ctx, f.a, resume)
	if err != nil || op.State != store.StateSucceeded {
		t.Fatalf("resume: %+v %v", op, err)
	}
	var resumed store.AgentSession
	json.Unmarshal(op.Result, &resumed)
	t.Cleanup(func() {
		f.svc.StopAgentSession(f.a, StopAgentSessionRequest{OperationID: "cleanup2", SessionID: resumed.ID})
	})
	if resumed.ID == sess.ID || resumed.ProtocolSessionID != sess.ProtocolSessionID {
		t.Fatalf("resumed = %+v, previous = %+v", resumed, sess)
	}
	resume.OperationID, resume.ResumeSessionID = "s3", resumed.ID
	if _, err := f.svc.StartAgentSession(ctx, f.a, resume); code(err) != CodeInvalidState {
		t.Fatalf("resume of an active session: %v", err)
	}
}
