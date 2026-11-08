package service

import (
	"context"
	"errors"
	"slices"

	"github.com/takezoh/syntoria-reach/src/internal/core"
)

// An agent session is an agent the workspace backend starts in a workspace:
// on Orca in a terminal of the workspace, whose handle is the session ID,
// and in a local workspace as a process of the daemon. Prompts go to the
// agent only while it waits for one, and its reply is its last message of
// the turn. Sessions are not owned by the client that started them.

// StartAgentSessionRequest starts an agent in a workspace.
type StartAgentSessionRequest struct {
	OperationID string `json:"operation_id"`
	Workspace   string `json:"workspace"`
	Agent       string `json:"agent"`
}

// StartAgentSession starts an agent once per operation ID. The result is the
// session, once the agent waits for its first prompt or the backend stopped
// waiting for it; a completed workspace is set back in progress.
func (s *Service) StartAgentSession(ctx context.Context, c *Client, req StartAgentSessionRequest) (*core.Operation, error) {
	rec := core.AuditRecord{ClientID: c.ID, Action: "start_agent_session", WorkspaceID: req.Workspace, Target: req.Agent, OperationID: req.OperationID}
	w, err := s.authorize(ctx, c, req.Workspace, core.PermAgentRun)
	if err == nil {
		defer w.release() // until the agent's session is open
	}
	i := slices.IndexFunc(s.agents, func(a core.Agent) bool { return a.ID == req.Agent })
	if err == nil && i < 0 {
		err = core.Errorf(core.CodePermissionDenied, "agent %q is not allowed", req.Agent)
	}
	if err != nil {
		s.audit(rec, err)
		return nil, err
	}
	ctx = context.WithoutCancel(ctx)
	return s.operation(c, "start_agent_session", req.OperationID, req, rec, func() (any, error) {
		sess, err := s.workspaces.StartAgent(ctx, w.Workspace, s.agents[i])
		if err != nil {
			return nil, err
		}
		s.reopen(ctx, w.Workspace)
		return sess, nil
	})
}

// SendAgentPromptRequest sends a prompt to a session.
type SendAgentPromptRequest struct {
	OperationID string `json:"operation_id"`
	SessionID   string `json:"session_id"`
	Text        string `json:"text"`
}

// PromptResult is the result of a delivered prompt: the stages the backend
// observed.
type PromptResult struct {
	SessionID string   `json:"session_id"`
	Stages    []string `json:"stages"`
}

// SendAgentPrompt delivers a prompt to the session's agent, once per
// operation ID, and returns once the agent started its turn. A prompt is
// sent only while the agent waits for one.
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
		st, err := s.session(ctx, req.SessionID)
		if err != nil {
			return nil, err
		}
		release, ok := s.use(st.Workspace)
		if !ok {
			return nil, core.Errorf(core.CodeInvalidState, "workspace is being removed")
		}
		defer release()
		// Without an agent, an Orca terminal runs a shell, which would run
		// the prompt as a command. While the agent asks its user something,
		// such as a permission, or Orca cannot tell, the prompt would answer
		// it; the answer is the user's, in Orca. During a turn the agent
		// could ask just before the prompt arrives.
		switch st.State {
		case core.AgentExited:
			return nil, core.Errorf(core.CodeInvalidState, "the agent of session %q is no longer running", req.SessionID)
		case core.AgentWaitingOnHuman:
			return nil, core.Errorf(core.CodeInvalidState, "the agent may be waiting for the user in Orca: %s", st.WaitingOnHuman)
		case core.AgentWorking:
			return nil, core.Errorf(core.CodeInvalidState, "the agent is not waiting for a prompt: read_agent_output until state is idle")
		}
		stages, err := s.workspaces.Prompt(ctx, req.SessionID, req.Text)
		if err != nil {
			return nil, err
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
// last prompt, empty when there is none. ReplyTruncated says the reply
// reached the length the backend keeps and may be cut.
type AgentOutput struct {
	State          string `json:"state"`
	WaitingOnHuman string `json:"waiting_on_human,omitempty"`
	Reply          string `json:"reply"`
	ReplyTruncated bool   `json:"reply_truncated,omitempty"`
}

// ReadAgentOutput reads what the agent of a session does, and its reply.
func (s *Service) ReadAgentOutput(ctx context.Context, c *Client, req ReadAgentOutputRequest) (out *AgentOutput, err error) {
	rec := core.AuditRecord{ClientID: c.ID, Action: "read_agent_output", Target: req.SessionID}
	defer func() { s.audit(rec, err) }()
	if !slices.Contains(c.perms, core.PermAgentRun) {
		return nil, core.Errorf(core.CodePermissionDenied, "%s is not allowed", core.PermAgentRun)
	}
	st, err := s.session(ctx, req.SessionID)
	if err != nil {
		return nil, err
	}
	rec.WorkspaceID = st.Workspace
	out = &AgentOutput{State: st.State, WaitingOnHuman: st.WaitingOnHuman}
	if st.State == core.AgentIdle {
		out.Reply, out.ReplyTruncated = st.Reply, st.ReplyTruncated
	}
	return out, nil
}

// AgentSessionRequest identifies a session.
type AgentSessionRequest struct {
	SessionID string `json:"session_id"`
}

// GetAgentSession returns a session as the backend observes it.
func (s *Service) GetAgentSession(ctx context.Context, c *Client, req AgentSessionRequest) (a *core.AgentSession, err error) {
	rec := core.AuditRecord{ClientID: c.ID, Action: "get_agent_session", Target: req.SessionID}
	defer func() { s.audit(rec, err) }()
	if !slices.Contains(c.perms, core.PermAgentRun) {
		return nil, core.Errorf(core.CodePermissionDenied, "%s is not allowed", core.PermAgentRun)
	}
	st, err := s.session(ctx, req.SessionID)
	if err != nil {
		return nil, err
	}
	rec.WorkspaceID = st.Workspace
	return &st.AgentSession, nil
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

// StopAgentSession ends the session and its agent (on Orca, it closes the
// terminal), once per operation ID.
func (s *Service) StopAgentSession(ctx context.Context, c *Client, req StopAgentSessionRequest) (*core.Operation, error) {
	rec := core.AuditRecord{ClientID: c.ID, Action: "stop_agent_session", Target: req.SessionID, OperationID: req.OperationID}
	if !slices.Contains(c.perms, core.PermAgentRun) {
		err := core.Errorf(core.CodePermissionDenied, "%s is not allowed", core.PermAgentRun)
		s.audit(rec, err)
		return nil, err
	}
	ctx = context.WithoutCancel(ctx)
	return s.operation(c, "stop_agent_session", req.OperationID, req, rec, func() (any, error) {
		err := s.workspaces.Stop(ctx, req.SessionID)
		switch {
		case errors.Is(err, core.ErrNotFound):
			return nil, sessionNotFound(req.SessionID)
		case err != nil:
			return nil, err
		}
		return StopResult{SessionID: req.SessionID, Stopped: true}, nil
	})
}

// session reads session id from the backend; a session that is gone, or
// that is not one of Reach's, is not found.
func (s *Service) session(ctx context.Context, id string) (*core.AgentStatus, error) {
	st, err := s.workspaces.Session(ctx, id)
	if errors.Is(err, core.ErrNotFound) {
		return nil, sessionNotFound(id)
	}
	if err != nil {
		return nil, classify(err)
	}
	return st, nil
}

func sessionNotFound(id string) error {
	return core.Errorf(core.CodeNotFound, "agent session %q not found", id)
}
