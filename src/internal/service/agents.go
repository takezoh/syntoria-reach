package service

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"time"

	"github.com/takezoh/syntoria-reach/src/internal/core"
)

// An agent session is an agent Reach starts in an Orca terminal of a
// workspace: the agent's configured command is typed into a new terminal,
// prompts are typed into that terminal, and the agent's reply is read from
// Orca's agent status. The session ID is the terminal's Orca handle; Reach
// keeps nothing about sessions, which outlive the daemon and end when the
// terminal closes. Every terminal in a Reach workspace is a session, and no
// other terminal is.

const (
	// startWait is how long starting a session waits for the agent to wait
	// for its first prompt; the remote gateway gives a request 30 s.
	startWait = 15 * time.Second
	// idleWait is how long Reach looks for the agent waiting for a prompt
	// before it reports the agent working.
	idleWait = 3 * time.Second
)

// StartAgentSessionRequest starts an agent in a workspace.
type StartAgentSessionRequest struct {
	OperationID string `json:"operation_id"`
	Workspace   string `json:"workspace"`
	Agent       string `json:"agent"`
}

// StartAgentSession starts an agent once per operation ID. The result is the
// session, once the agent waits for its first prompt or startWait passed; a
// completed workspace is set back in progress.
func (s *Service) StartAgentSession(ctx context.Context, c *Client, req StartAgentSessionRequest) (*core.Operation, error) {
	rec := core.AuditRecord{ClientID: c.ID, Action: "start_agent_session", WorkspaceID: req.Workspace, Target: req.Agent, OperationID: req.OperationID}
	w, err := s.authorize(ctx, c, req.Workspace, core.PermAgentRun)
	if err == nil {
		defer w.release() // until the agent's terminal is open
	}
	a, known := s.harness[req.Agent]
	if err == nil && !known {
		err = core.Errorf(core.CodePermissionDenied, "agent %q is not allowed", req.Agent)
	}
	if err != nil {
		s.audit(rec, err)
		return nil, err
	}
	ctx = context.WithoutCancel(ctx)
	return s.operation(c, "start_agent_session", req.OperationID, req, rec, func() (any, error) {
		_, root, err := s.workspace(ctx, req.Workspace)
		if err != nil {
			return nil, err
		}
		handle, err := s.orca.CreateTerminal(ctx, root.ID, "reach "+req.Agent, core.ShellCommand(a.Command))
		if err != nil {
			return nil, orcaError(err)
		}
		// Until the agent is up, the terminal runs only the shell.
		_, _, err = s.orca.WaitIdle(ctx, handle, startWait)
		var sess *core.AgentSession
		switch {
		case errors.Is(err, core.ErrNotFound):
			err = core.Errorf(core.CodeHarnessFailed, "the terminal of agent %q closed as the agent started", req.Agent)
		case err != nil:
			err = orcaError(err)
		default:
			sess, err = s.agentSession(ctx, handle)
			if err == nil && sess.State == core.AgentExited {
				err = core.Errorf(core.CodeHarnessFailed, "agent %q did not start in its terminal; check its command", req.Agent)
			}
		}
		if err != nil {
			if cerr := s.orca.CloseTerminal(ctx, handle); cerr != nil {
				slog.Warn("cannot close the terminal of an agent that failed to start", "terminal", handle, "err", cerr)
			}
			return nil, err
		}
		s.reopen(ctx, req.Workspace)
		return sess, nil
	})
}

// SendAgentPromptRequest sends a prompt to a session.
type SendAgentPromptRequest struct {
	OperationID string `json:"operation_id"`
	SessionID   string `json:"session_id"`
	Text        string `json:"text"`
}

// PromptResult is the result of a delivered prompt: the stages Orca
// observed.
type PromptResult struct {
	SessionID string   `json:"session_id"`
	Stages    []string `json:"stages"`
}

// SendAgentPrompt types a prompt into the session's terminal and submits
// it, once per operation ID, and returns once the agent started its turn. A
// prompt is sent only while the agent waits for one.
func (s *Service) SendAgentPrompt(ctx context.Context, c *Client, req SendAgentPromptRequest) (*core.Operation, error) {
	rec := core.AuditRecord{ClientID: c.ID, Action: "send_agent_prompt", Target: req.SessionID, OperationID: req.OperationID}
	var err error
	switch {
	case !slices.Contains(c.perms, core.PermAgentRun):
		err = core.Errorf(core.CodePermissionDenied, "%s is not allowed", core.PermAgentRun)
	default:
		err = core.CheckPrompt(req.Text)
	}
	if err != nil {
		s.audit(rec, err)
		return nil, err
	}
	ctx = context.WithoutCancel(ctx)
	return s.operation(c, "send_agent_prompt", req.OperationID, req, rec, func() (any, error) {
		t, release, err := s.useSession(ctx, req.SessionID)
		if err != nil {
			return nil, err
		}
		defer release()
		// Without an agent the terminal runs a shell, which would run the
		// prompt as a command. While the agent asks its user something, such
		// as a permission, or Orca cannot tell, the prompt would answer it;
		// the answer is the user's, in Orca. During a turn the agent could
		// ask just before the prompt arrives.
		state, _, err := s.agentState(ctx, t)
		if err != nil {
			return nil, err
		}
		switch state {
		case core.AgentExited:
			return nil, core.Errorf(core.CodeInvalidState, "the agent of session %q is no longer running in its terminal", req.SessionID)
		case core.AgentWaitingOnHuman:
			return nil, core.Errorf(core.CodeInvalidState, "the agent may be waiting for the user in Orca: %s", t.WaitingOnHuman)
		case core.AgentWorking:
			return nil, core.Errorf(core.CodeInvalidState, "the agent is not waiting for a prompt: read_agent_output until state is idle")
		}
		stages, err := s.orca.SendPrompt(ctx, req.SessionID, req.Text)
		if err != nil {
			return nil, orcaError(err)
		}
		if !slices.Contains(stages, "turn_started") {
			return nil, core.Errorf(core.CodeHarnessFailed, "the agent did not start a turn (Orca observed %v); it may be waiting for input in Orca", stages)
		}
		return PromptResult{SessionID: req.SessionID, Stages: stages}, nil
	})
}

// ReadAgentOutputRequest reads what the agent of a session does and its
// reply.
type ReadAgentOutputRequest struct {
	SessionID string `json:"session_id"`
}

// AgentOutput is what the agent of a session does and, once it waits for a
// prompt (State is idle), its last message of the turn: its reply to the
// last prompt, empty when Orca reports none. ReplyTruncated says the reply
// reached the length Orca keeps and may be cut.
type AgentOutput struct {
	State          string `json:"state"`
	WaitingOnHuman string `json:"waiting_on_human,omitempty"`
	Reply          string `json:"reply"`
	ReplyTruncated bool   `json:"reply_truncated,omitempty"`
}

// ReadAgentOutput reads what the agent of a session does, and its reply,
// from Orca.
func (s *Service) ReadAgentOutput(ctx context.Context, c *Client, req ReadAgentOutputRequest) (out *AgentOutput, err error) {
	rec := core.AuditRecord{ClientID: c.ID, Action: "read_agent_output", Target: req.SessionID}
	defer func() { s.audit(rec, err) }()
	if !slices.Contains(c.perms, core.PermAgentRun) {
		return nil, core.Errorf(core.CodePermissionDenied, "%s is not allowed", core.PermAgentRun)
	}
	t, release, err := s.useSession(ctx, req.SessionID)
	if err != nil {
		return nil, err
	}
	defer release()
	rec.WorkspaceID = core.WorkspaceOfEntry(t.WorktreeID)
	state, turn, err := s.agentState(ctx, t)
	if err != nil {
		return nil, err
	}
	out = &AgentOutput{State: state, WaitingOnHuman: t.WaitingOnHuman}
	if state == core.AgentIdle && turn != nil {
		out.Reply, out.ReplyTruncated = turn.Reply, turn.Truncated
	}
	return out, nil
}

// AgentSessionRequest identifies a session.
type AgentSessionRequest struct {
	SessionID string `json:"session_id"`
}

// GetAgentSession returns a session as Orca reports it.
func (s *Service) GetAgentSession(ctx context.Context, c *Client, req AgentSessionRequest) (a *core.AgentSession, err error) {
	rec := core.AuditRecord{ClientID: c.ID, Action: "get_agent_session", Target: req.SessionID}
	defer func() { s.audit(rec, err) }()
	if !slices.Contains(c.perms, core.PermAgentRun) {
		return nil, core.Errorf(core.CodePermissionDenied, "%s is not allowed", core.PermAgentRun)
	}
	a, err = s.agentSession(ctx, req.SessionID)
	if a != nil {
		rec.WorkspaceID = a.Workspace
	}
	return a, err
}

// StopAgentSessionRequest ends a session.
type StopAgentSessionRequest struct {
	OperationID string `json:"operation_id"`
	SessionID   string `json:"session_id"`
}

// StopResult is the result of a stopped session.
type StopResult struct {
	SessionID string `json:"session_id"`
	Stopped   bool   `json:"stopped"`
}

// StopAgentSession closes the session's terminal, which ends the agent, once
// per operation ID.
func (s *Service) StopAgentSession(ctx context.Context, c *Client, req StopAgentSessionRequest) (*core.Operation, error) {
	rec := core.AuditRecord{ClientID: c.ID, Action: "stop_agent_session", Target: req.SessionID, OperationID: req.OperationID}
	if !slices.Contains(c.perms, core.PermAgentRun) {
		err := core.Errorf(core.CodePermissionDenied, "%s is not allowed", core.PermAgentRun)
		s.audit(rec, err)
		return nil, err
	}
	ctx = context.WithoutCancel(ctx)
	return s.operation(c, "stop_agent_session", req.OperationID, req, rec, func() (any, error) {
		_, release, err := s.useSession(ctx, req.SessionID)
		if err != nil {
			return nil, err
		}
		defer release()
		if err := s.orca.CloseTerminal(ctx, req.SessionID); err != nil {
			return nil, orcaError(err)
		}
		return StopResult{SessionID: req.SessionID, Stopped: true}, nil
	})
}

// agentSession reads session id from Orca, with what its agent does.
func (s *Service) agentSession(ctx context.Context, id string) (*core.AgentSession, error) {
	t, release, err := s.useSession(ctx, id)
	if err != nil {
		return nil, err
	}
	defer release()
	state, _, err := s.agentState(ctx, t)
	if err != nil {
		return nil, err
	}
	return &core.AgentSession{ID: id, Workspace: core.WorkspaceOfEntry(t.WorktreeID), Agent: t.AgentIdentity, State: state, WaitingOnHuman: t.WaitingOnHuman}, nil
}

// agentState tells what the agent in terminal t does and, when it waits for
// a prompt, returns its latest turn as Orca's agent status reports it (nil
// when Orca reports none). Orca tells a question to the user from a turn
// only through what it saw of the wait, and an agent waiting for a prompt
// only by seeing it idle within idleWait; until the agent's status hooks
// report the turn ended, its reply is not there yet, so the agent is still
// working.
func (s *Service) agentState(ctx context.Context, t *core.TerminalStatus) (string, *core.AgentTurn, error) {
	switch {
	case t.AgentIdentity == "":
		return core.AgentExited, nil, nil
	case t.WaitingOnHuman != "":
		return core.AgentWaitingOnHuman, nil, nil
	}
	idle, blocked, err := s.orca.WaitIdle(ctx, t.Handle, idleWait)
	switch {
	case errors.Is(err, core.ErrNotFound):
		return "", nil, sessionNotFound(t.Handle)
	case err != nil:
		return "", nil, orcaError(err)
	case blocked != "":
		t.WaitingOnHuman = blocked
		return core.AgentWaitingOnHuman, nil, nil
	case !idle:
		return core.AgentWorking, nil, nil
	}
	var turn *core.AgentTurn
	if key := t.PaneKey(); key != "" {
		if turn, err = s.orca.AgentTurn(ctx, key); err != nil {
			return "", nil, orcaError(err)
		}
	}
	if turn != nil && turn.Running {
		return core.AgentWorking, nil, nil
	}
	return core.AgentIdle, turn, nil
}

// session reads the terminal of session id from Orca. It must be open in a
// Reach workspace; otherwise the session is not found.
func (s *Service) session(ctx context.Context, id string) (*core.TerminalStatus, error) {
	t, err := s.orca.ShowTerminal(ctx, id)
	if errors.Is(err, core.ErrNotFound) {
		return nil, sessionNotFound(id)
	}
	if err != nil {
		return nil, orcaError(err)
	}
	if !t.Connected || t.Handle != id {
		return nil, sessionNotFound(id)
	}
	if _, err := s.repo(ctx, core.WorkspaceOfEntry(t.WorktreeID)); err != nil {
		return nil, sessionNotFound(id)
	}
	return t, nil
}

// useSession reads session id like session and holds its workspace, as a
// request holding it, until release; it fails while cleanup removes the
// workspace.
func (s *Service) useSession(ctx context.Context, id string) (*core.TerminalStatus, func(), error) {
	t, err := s.session(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	release, ok := s.use(core.WorkspaceOfEntry(t.WorktreeID))
	if !ok {
		return nil, nil, core.Errorf(core.CodeInvalidState, "workspace is being removed")
	}
	return t, release, nil
}

func sessionNotFound(id string) error {
	return core.Errorf(core.CodeNotFound, "agent session %q not found", id)
}
