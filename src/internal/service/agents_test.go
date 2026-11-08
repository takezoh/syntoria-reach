package service_test

import (
	"context"
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

func (f *fixture) read(t *testing.T, session, cursor string, limit int) *service.AgentOutput {
	t.Helper()
	o, err := f.svc.ReadAgentOutput(context.Background(), f.a, service.ReadAgentOutputRequest{SessionID: session, Cursor: cursor, Limit: limit})
	if err != nil {
		t.Fatal(err)
	}
	return o
}

// sends counts the prompts typed into terminal handle so far.
func (f *fixture) sends(handle string) (n int) {
	for _, c := range f.fakes.Orca.Calls() {
		if strings.HasPrefix(c, "send "+handle+" ") {
			n++
		}
	}
	return n
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
	// which is the session; it is returned once the agent waits for a
	// prompt.
	s := f.startSession(t, "s1", service.StartAgentSessionRequest{})
	if s.Workspace != f.ws || s.ID == "" || s.Agent == "" || s.State != core.AgentIdle {
		t.Fatalf("session = %+v", s)
	}
	if cmd, open := f.fakes.Orca.TerminalCommand(s.ID); cmd != "/bin/codex --model 'gpt x'" || !open {
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

	// An agent that does not come up leaves no terminal behind.
	f.fakes.Orca.NoAgent = true
	op, err := f.svc.StartAgentSession(ctx, f.a, service.StartAgentSessionRequest{OperationID: "s2", Workspace: f.ws, Agent: "dsh"})
	if err != nil || op.State != core.StateFailed || op.ErrorCode != core.CodeHarnessFailed {
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

	if o := f.read(t, s.ID, "", 0); len(o.Lines) != 0 || o.Cursor != "0" || o.State != core.AgentIdle {
		t.Fatalf("before any prompt: %+v", o)
	}
	op := f.prompt(t, "p1", s.ID, "hello")
	if r := result[service.PromptResult](t, op); op.State != core.StateSucceeded || strings.Join(r.Stages, ",") != "input_accepted,turn_started" {
		t.Fatalf("prompt: %+v", op)
	}
	o := f.read(t, s.ID, "", 0)
	if strings.Join(o.Lines, "|") != "you: hello|agent: echo: hello" || o.State != core.AgentIdle || o.More {
		t.Fatalf("output: %+v", o)
	}
	// The cursor reads on from there; a resent prompt is not typed again.
	if more := f.read(t, s.ID, o.Cursor, 0); len(more.Lines) != 0 || more.Cursor != o.Cursor {
		t.Fatalf("read on: %+v", more)
	}
	f.prompt(t, "p1", s.ID, "hello")
	if o := f.read(t, s.ID, "", 0); len(o.Lines) != 2 {
		t.Fatalf("the resent prompt was typed again: %q", o.Lines)
	}
	// A limited read says more follows.
	if o := f.read(t, s.ID, "", 1); len(o.Lines) != 1 || !o.More {
		t.Fatalf("limited read: %+v", o)
	}
	if _, err := f.svc.ReadAgentOutput(ctx, f.a, service.ReadAgentOutputRequest{SessionID: s.ID, Limit: 1001}); code(err) != core.CodeInvalidRequest {
		t.Fatalf("limit over the maximum: %v", err)
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
	if err != nil || got.ID != s.ID || got.Workspace != f.ws || got.Agent == "" || got.State != core.AgentIdle {
		t.Fatalf("get: %+v %v", got, err)
	}

	// Once the agent quit, its terminal runs a shell: a prompt is refused,
	// never typed there as a command.
	other := f.startSession(t, "s-other", service.StartAgentSessionRequest{Agent: "dsh"})
	f.fakes.Orca.ExitAgent(other.ID)
	if op := f.prompt(t, "p-shell", other.ID, "rm -rf ~"); op.State != core.StateFailed || op.ErrorCode != core.CodeInvalidState || f.sends(other.ID) != 0 {
		t.Fatalf("prompt to a shell: %+v, %d sends", op, f.sends(other.ID))
	}
	if got, err := f.svc.GetAgentSession(ctx, f.a, service.AgentSessionRequest{SessionID: other.ID}); err != nil || got.State != core.AgentExited || got.Agent != "" {
		t.Fatalf("get a session whose agent quit: %+v %v", got, err)
	}

	// Stopping closes the terminal, and with it the session.
	if op, err := f.svc.StopAgentSession(ctx, f.a, service.StopAgentSessionRequest{OperationID: "x", SessionID: s.ID}); err != nil || op.State != core.StateSucceeded {
		t.Fatalf("stop: %+v %v", op, err)
	}
	if _, open := f.fakes.Orca.TerminalCommand(s.ID); open {
		t.Fatal("the terminal is still open")
	}
	if op := f.prompt(t, "p4", s.ID, "late"); op.State != core.StateFailed || op.ErrorCode != core.CodeNotFound {
		t.Fatalf("prompt after stop: %+v", op)
	}
	if _, err := f.svc.ReadAgentOutput(ctx, f.a, service.ReadAgentOutputRequest{SessionID: s.ID}); code(err) != core.CodeNotFound {
		t.Fatalf("read after stop: %v", err)
	}
	if _, err := f.svc.GetAgentSession(ctx, f.a, service.AgentSessionRequest{SessionID: s.ID}); code(err) != core.CodeNotFound {
		t.Fatalf("get after stop: %v", err)
	}
}

// A prompt reaches the agent as a prompt: never as one of the agent's
// commands, nor as the answer to what the agent asks its user.
func TestPromptsReachOnlyTheAgent(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	s := f.startSession(t, "s", service.StartAgentSessionRequest{})
	for _, text := range []string{"!rm -rf ~", " /add-dir /", "x\x1b[201~y", strings.Repeat("a", core.MaxPromptBytes+1)} {
		if _, err := f.svc.SendAgentPrompt(ctx, f.a, service.SendAgentPromptRequest{OperationID: "p", SessionID: s.ID, Text: text}); code(err) != core.CodeInvalidRequest {
			t.Errorf("%.20q: %v", text, err)
		}
	}
	// What Orca saw the agent wait for from its user, whether in its status
	// or while waiting for it to be idle; or Orca could not tell.
	for name, ask := range map[string]func(){
		"asks-its-user":       func() { f.fakes.Orca.AskUser(s.ID, "Allow Bash(rm -rf ~)? (prompt-text)") },
		"orca-cannot-tell":    func() { f.fakes.Orca.AskUser(s.ID, core.WaitUnknown) },
		"stopped-at-a-prompt": func() { f.fakes.Orca.Block(s.ID, "permission") },
	} {
		ask()
		if op := f.prompt(t, "p-"+name, s.ID, "yes"); op.State != core.StateFailed || op.ErrorCode != core.CodeInvalidState {
			t.Errorf("%s: prompt %+v", name, op)
		}
		if o := f.read(t, s.ID, "", 0); o.State != core.AgentWaitingOnHuman || o.WaitingOnHuman == "" {
			t.Errorf("%s: read %+v", name, o)
		}
		f.fakes.Orca.AskUser(s.ID, "")
		f.fakes.Orca.Block(s.ID, "")
	}
	if n := f.sends(s.ID); n != 0 {
		t.Fatalf("%d prompts reached the terminal", n)
	}

	// During a turn the agent may ask its user before a prompt arrives.
	f.fakes.Orca.Busy(s.ID, true)
	if op := f.prompt(t, "p-busy", s.ID, "and then"); op.State != core.StateFailed || op.ErrorCode != core.CodeInvalidState || f.sends(s.ID) != 0 {
		t.Fatalf("prompt during a turn: %+v, %d sends", op, f.sends(s.ID))
	}
	if o := f.read(t, s.ID, "", 0); o.State != core.AgentWorking {
		t.Fatalf("read during a turn: %+v", o)
	}
	f.fakes.Orca.Busy(s.ID, false)
	if op := f.prompt(t, "p-done", s.ID, "and then"); op.State != core.StateSucceeded {
		t.Fatalf("prompt once the turn is done: %+v", op)
	}
}

// Reach drives only agents in its own workspaces: a terminal elsewhere in
// Orca is not a session.
func TestSessionsOutsideWorkspacesAreNotFound(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.fakes.Orca.AddRepo(core.Repo{ID: "git-1", DisplayName: "syntoria-reach", Path: "/src/syntoria-reach", Kind: "git"})
	foreign, _ := f.fakes.Orca.CreateTerminal(ctx, "git-1::/src/syntoria-reach", "", "claude")
	f.fakes.Orca.SendPrompt(ctx, foreign, "secret work")

	for _, id := range []string{foreign, "term-nope"} {
		if _, err := f.svc.GetAgentSession(ctx, f.a, service.AgentSessionRequest{SessionID: id}); code(err) != core.CodeNotFound {
			t.Errorf("get %s: %v", id, err)
		}
		if _, err := f.svc.ReadAgentOutput(ctx, f.a, service.ReadAgentOutputRequest{SessionID: id}); code(err) != core.CodeNotFound {
			t.Errorf("read %s: %v", id, err)
		}
		if op := f.prompt(t, "p-"+id, id, "do something"); op.ErrorCode != core.CodeNotFound {
			t.Errorf("prompt %s: %+v", id, op)
		}
		if op, err := f.svc.StopAgentSession(ctx, f.a, service.StopAgentSessionRequest{OperationID: "x-" + id, SessionID: id}); err != nil || op.ErrorCode != core.CodeNotFound {
			t.Errorf("stop %s: %+v %v", id, op, err)
		}
	}
	if _, open := f.fakes.Orca.TerminalCommand(foreign); !open || f.sends(foreign) != 1 {
		t.Errorf("the foreign terminal was touched: open %v, %d sends", open, f.sends(foreign))
	}
	if _, err := f.svc.ReadAgentOutput(ctx, f.b, service.ReadAgentOutputRequest{SessionID: foreign}); code(err) != core.CodePermissionDenied {
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
