package service_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/takezoh/syntoria-reach/src/internal/core"
	"github.com/takezoh/syntoria-reach/src/internal/service"
)

// startSession starts an agent session for client a, in the fixture's
// workspace and with codex unless req says otherwise.
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

func (f *fixture) prompt(t *testing.T, opID, session, text string) *core.Operation {
	t.Helper()
	op, err := f.svc.SendAgentPrompt(context.Background(), f.a, service.SendAgentPromptRequest{OperationID: opID, SessionID: session, Text: text})
	if err != nil {
		t.Fatal(err)
	}
	return op
}

func (f *fixture) read(t *testing.T, session, cursor string) *service.AgentMessages {
	t.Helper()
	m, err := f.svc.ReadAgentMessages(context.Background(), f.a, service.ReadAgentMessagesRequest{SessionID: session, Cursor: cursor})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func text(t *testing.T, m json.RawMessage) string {
	t.Helper()
	var msg struct {
		Role   string `json:"role"`
		Blocks []struct {
			Text string `json:"text"`
		} `json:"blocks"`
	}
	if err := json.Unmarshal(m, &msg); err != nil || len(msg.Blocks) != 1 {
		t.Fatalf("message %s: %v", m, err)
	}
	return msg.Role + ": " + msg.Blocks[0].Text
}

func TestStartAgentSession(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	for name, c := range map[string]struct {
		client *service.Client
		req    service.StartAgentSessionRequest
		code   string
	}{
		"unknown agent":         {f.a, service.StartAgentSessionRequest{Workspace: f.ws, Agent: "nope"}, core.CodePermissionDenied},
		"client without agents": {f.b, service.StartAgentSessionRequest{Workspace: f.ws, Agent: "codex"}, core.CodePermissionDenied},
		"unknown workspace":     {f.a, service.StartAgentSessionRequest{Workspace: "nope", Agent: "codex"}, core.CodeNotFound},
	} {
		c.req.OperationID = "s"
		if _, err := f.svc.StartAgentSession(ctx, c.client, c.req); code(err) != c.code {
			t.Errorf("%s: %v", name, err)
		}
	}

	// The agent's command runs in a new Orca terminal of the workspace,
	// which becomes the session's Dispatch.
	s := f.startSession(t, "s1", service.StartAgentSessionRequest{})
	if s.Workspace != f.ws || s.Terminal == "" || s.ID == "" {
		t.Fatalf("session = %+v", s)
	}
	if cmd, open := f.fakes.Orca.TerminalCommand(s.Terminal); cmd != "/bin/codex --model 'gpt x'" || !open {
		t.Fatalf("terminal runs %q (open %v)", cmd, open)
	}
	if root := f.fakes.Orca.Root(f.ws); !strings.Contains(strings.Join(f.fakes.Orca.Calls(), "|"), "terminal "+root.ID+" /bin/codex") {
		t.Fatalf("the terminal is not in the workspace: %q", f.fakes.Orca.Calls())
	}
	// Resending the start returns the same session without another terminal.
	terminals := func() (n int) {
		for _, c := range f.fakes.Orca.Calls() {
			if strings.HasPrefix(c, "terminal ") {
				n++
			}
		}
		return n
	}
	if again := f.startSession(t, "s1", service.StartAgentSessionRequest{}); again.ID != s.ID || terminals() != 1 {
		t.Fatalf("resend: %+v, %d terminals", again, terminals())
	}

	// Orca not adopting the terminal leaves no agent running.
	f.fakes.Orca.FailAdopt = true
	op, err := f.svc.StartAgentSession(ctx, f.a, service.StartAgentSessionRequest{OperationID: "s2", Workspace: f.ws, Agent: "dsh"})
	if err != nil || op.State != core.StateFailed || op.ErrorCode != core.CodeOrcaFailed {
		t.Fatalf("failed start: %+v %v", op, err)
	}
	calls := f.fakes.Orca.Calls()
	if last := calls[len(calls)-1]; !strings.HasPrefix(last, "close ") {
		t.Fatalf("the terminal of a failed start was left open: %q", calls)
	}
}

func TestAgentConversation(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	s := f.startSession(t, "s", service.StartAgentSessionRequest{})

	// Nothing to read until the agent reported its session.
	if m := f.read(t, s.ID, ""); len(m.Messages) != 0 || m.Cursor != "" {
		t.Fatalf("before any prompt: %+v", m)
	}
	op := f.prompt(t, "p1", s.ID, "hello")
	if r := result[service.PromptResult](t, op); op.State != core.StateSucceeded || strings.Join(r.Stages, ",") != "input_accepted,turn_started" {
		t.Fatalf("prompt: %+v", op)
	}
	m := f.read(t, s.ID, "")
	if len(m.Messages) != 2 || text(t, m.Messages[0]) != "user: hello" || text(t, m.Messages[1]) != "assistant: echo: hello" || m.Activity != "done" || m.Terminal != "running" {
		t.Fatalf("conversation: %+v", m)
	}
	// The cursor reads on from there; a resent prompt is not typed again.
	if more := f.read(t, s.ID, m.Cursor); len(more.Messages) != 0 || more.Cursor != m.Cursor {
		t.Fatalf("read on: %+v", more)
	}
	f.prompt(t, "p1", s.ID, "hello")
	if m := f.read(t, s.ID, ""); len(m.Messages) != 2 {
		t.Fatalf("the resent prompt was typed again: %d messages", len(m.Messages))
	}
	if _, err := f.svc.SendAgentPrompt(ctx, f.a, service.SendAgentPromptRequest{OperationID: "p2", SessionID: s.ID, Text: " "}); code(err) != core.CodeInvalidRequest {
		t.Fatalf("empty prompt: %v", err)
	}
	// An agent that does not start the turn (a question in its terminal) is
	// an error, not a delivered prompt.
	f.fakes.Orca.SilentAgent = true
	if op := f.prompt(t, "p3", s.ID, "again"); op.State != core.StateFailed || op.ErrorCode != core.CodeHarnessFailed {
		t.Fatalf("silent agent: %+v", op)
	}
	f.fakes.Orca.SilentAgent = false

	got, err := f.svc.GetAgentSession(ctx, f.a, service.AgentSessionRequest{SessionID: s.ID})
	if err != nil || got.Workspace != f.ws || got.Terminal != s.Terminal || got.Agent == "" || got.Liveness != "live" {
		t.Fatalf("get: %+v %v", got, err)
	}

	// Once the agent quit, its terminal runs a shell: a prompt is refused,
	// never typed there as a command.
	other := f.startSession(t, "s-other", service.StartAgentSessionRequest{Agent: "dsh"})
	f.fakes.Orca.ExitAgent(other.Terminal)
	sends := func() (n int) {
		for _, c := range f.fakes.Orca.Calls() {
			if strings.HasPrefix(c, "send "+other.Terminal+" ") {
				n++
			}
		}
		return n
	}
	if op := f.prompt(t, "p-shell", other.ID, "rm -rf ~"); op.State != core.StateFailed || op.ErrorCode != core.CodeInvalidState || sends() != 0 {
		t.Fatalf("prompt to a shell: %+v, %d sends", op, sends())
	}

	// Stopping closes the terminal, and with it the session: Orca no longer
	// reads its transcript or knows its workspace.
	if op, err := f.svc.StopAgentSession(ctx, f.a, service.StopAgentSessionRequest{OperationID: "x", SessionID: s.ID}); err != nil || op.State != core.StateSucceeded {
		t.Fatalf("stop: %+v %v", op, err)
	}
	if _, open := f.fakes.Orca.TerminalCommand(s.Terminal); open {
		t.Fatal("the terminal is still open")
	}
	if op := f.prompt(t, "p4", s.ID, "late"); op.State != core.StateFailed || op.ErrorCode != core.CodeNotFound {
		t.Fatalf("prompt after stop: %+v", op)
	}
	if _, err := f.svc.ReadAgentMessages(ctx, f.a, service.ReadAgentMessagesRequest{SessionID: s.ID}); code(err) != core.CodeNotFound {
		t.Fatalf("read after stop: %v", err)
	}
	if _, err := f.svc.GetAgentSession(ctx, f.a, service.AgentSessionRequest{SessionID: s.ID}); code(err) != core.CodeNotFound {
		t.Fatalf("get after stop: %v", err)
	}
}

// Reach drives only agents in its own workspaces: a Dispatch elsewhere in
// Orca is not a session.
func TestSessionsOutsideWorkspacesAreNotFound(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.fakes.Orca.AddRepo(core.Repo{ID: "git-1", DisplayName: "syntoria-reach", Path: "/src/syntoria-reach", Kind: "git"})
	h, _ := f.fakes.Orca.CreateTerminal(ctx, "git-1::/src/syntoria-reach", "", "claude")
	foreign, _ := f.fakes.Orca.AdoptTerminal(ctx, h, "someone's agent")
	f.fakes.Orca.SendPrompt(ctx, h, "secret work")

	for _, id := range []string{foreign, "ctx-nope"} {
		if _, err := f.svc.GetAgentSession(ctx, f.a, service.AgentSessionRequest{SessionID: id}); code(err) != core.CodeNotFound {
			t.Errorf("get %s: %v", id, err)
		}
		if _, err := f.svc.ReadAgentMessages(ctx, f.a, service.ReadAgentMessagesRequest{SessionID: id}); code(err) != core.CodeNotFound {
			t.Errorf("read %s: %v", id, err)
		}
		if op := f.prompt(t, "p-"+id, id, "do something"); op.ErrorCode != core.CodeNotFound {
			t.Errorf("prompt %s: %+v", id, op)
		}
	}
	if _, err := f.svc.ReadAgentMessages(ctx, f.b, service.ReadAgentMessagesRequest{SessionID: foreign}); code(err) != core.CodePermissionDenied {
		t.Errorf("client without agent.run: %v", err)
	}
}

// An open terminal, such as an agent's, keeps a completed workspace from
// cleanup.
func TestCleanupSkipsWorkspacesWithOpenTerminals(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	now := time.Now()
	old := now.Add(-core.CleanupAfter - time.Hour)
	f.fakes.Store.Now = func() time.Time { return old }
	s := f.startSession(t, "s", service.StartAgentSessionRequest{})
	f.svc.UpdateWorkspace(ctx, f.a, service.UpdateWorkspaceRequest{OperationID: "done", Workspace: f.ws, Status: "completed"})
	f.fakes.Orca.Touch(f.ws, old)

	if removed, err := f.svc.Cleanup(ctx, now); err != nil || len(removed) != 0 {
		t.Fatalf("removed a workspace with an open agent terminal: %q %v", removed, err)
	}
	f.svc.StopAgentSession(ctx, f.a, service.StopAgentSessionRequest{OperationID: "x", SessionID: s.ID})
	if removed, err := f.svc.Cleanup(ctx, now); err != nil || len(removed) != 1 {
		t.Fatalf("after the agent stopped: %q %v", removed, err)
	}
}
