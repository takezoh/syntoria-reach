package service

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"

	"github.com/takezoh/syntoria-reach/src/internal/core"
)

// An agent session is an agent Reach starts in an Orca terminal of a
// workspace: the agent's configured command is typed into a new terminal,
// and the terminal is adopted as an Orca Dispatch, through which Orca reads
// the agent's transcript. Prompts are typed into the terminal. The session
// ID is the Dispatch ID; Reach keeps nothing about sessions, which outlive
// the daemon, and only sessions whose terminal runs in a Reach workspace are
// Reach's.

// StartAgentSessionRequest starts an agent in a workspace.
type StartAgentSessionRequest struct {
	OperationID string `json:"operation_id"`
	Workspace   string `json:"workspace"`
	Agent       string `json:"agent"`
}

// StartAgentSession starts an agent once per operation ID. The result is the
// session; a completed workspace is set back in progress.
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
		title := "reach " + req.Agent
		handle, err := s.orca.CreateTerminal(ctx, root.ID, title, core.ShellCommand(a.Command))
		if err != nil {
			return nil, orcaError(err)
		}
		id, err := s.orca.AdoptTerminal(ctx, handle, title)
		if err != nil {
			if cerr := s.orca.CloseTerminal(ctx, handle); cerr != nil {
				slog.Warn("cannot close the terminal of an agent Orca could not adopt", "terminal", handle, "err", cerr)
			}
			return nil, orcaError(err)
		}
		s.reopen(ctx, req.Workspace)
		return &core.AgentSession{ID: id, Workspace: req.Workspace, Terminal: handle}, nil
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
// prompt sent during a turn is queued by the agent.
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
		sess, release, err := s.useSession(ctx, req.SessionID)
		if err != nil {
			return nil, err
		}
		defer release()
		switch {
		case sess.Terminal == "":
			return nil, core.Errorf(core.CodeInvalidState, "session %q has ended", req.SessionID)
		case sess.Agent == "":
			// Without an agent the terminal runs a shell, which would run the
			// prompt as a command.
			return nil, core.Errorf(core.CodeInvalidState, "the agent of session %q is no longer running in its terminal", req.SessionID)
		case sess.WaitingOnHuman != "" && sess.WaitingOnHuman != "unknown":
			// Orca saw the agent ask its user something, such as a permission;
			// the answer is the user's, in Orca.
			return nil, core.Errorf(core.CodeInvalidState, "the agent is waiting for the user in Orca: %s", sess.WaitingOnHuman)
		}
		stages, err := s.orca.SendPrompt(ctx, sess.Terminal, req.Text)
		if err != nil {
			return nil, orcaError(err)
		}
		if !slices.Contains(stages, "turn_started") {
			return nil, core.Errorf(core.CodeHarnessFailed, "the agent did not start a turn (Orca observed %v); it may be waiting for input in Orca", stages)
		}
		return PromptResult{SessionID: req.SessionID, Stages: stages}, nil
	})
}

// ReadAgentMessagesRequest reads a session's conversation after Cursor (from
// the start when empty).
type ReadAgentMessagesRequest struct {
	SessionID string `json:"session_id"`
	Cursor    string `json:"cursor"`
	Limit     int    `json:"limit"`
}

// AgentMessages is a part of a session's conversation, in Orca's form, with
// the cursor to read on from and the agent's status. Activity is "done"
// while the agent waits for a prompt.
type AgentMessages struct {
	Messages []json.RawMessage `json:"messages"`
	Cursor   string            `json:"cursor"`
	Activity string            `json:"activity,omitempty"`
	Terminal string            `json:"terminal,omitempty"`
	Liveness string            `json:"liveness,omitempty"`
}

const maxAgentMessages = 1000

// ReadAgentMessages reads a session's transcript from Orca.
func (s *Service) ReadAgentMessages(ctx context.Context, c *Client, req ReadAgentMessagesRequest) (out *AgentMessages, err error) {
	rec := core.AuditRecord{ClientID: c.ID, Action: "read_agent_messages", Target: req.SessionID}
	defer func() { s.audit(rec, err) }()
	switch {
	case !slices.Contains(c.perms, core.PermAgentRun):
		return nil, core.Errorf(core.CodePermissionDenied, "%s is not allowed", core.PermAgentRun)
	case req.Limit < 0 || req.Limit > maxAgentMessages:
		return nil, core.Errorf(core.CodeInvalidRequest, "limit must be between 0 and %d", maxAgentMessages)
	}
	tr, err := s.orca.Transcript(ctx, req.SessionID, req.Cursor, req.Limit)
	if errors.Is(err, core.ErrTranscriptUnavailable) {
		// The agent has not reported its session yet: nothing to read.
		sess, release, err := s.useSession(ctx, req.SessionID)
		if err != nil {
			return nil, err
		}
		release()
		rec.WorkspaceID = sess.Workspace
		return &AgentMessages{Messages: []json.RawMessage{}, Cursor: req.Cursor, Liveness: sess.Liveness}, nil
	}
	if errors.Is(err, core.ErrNotFound) {
		return nil, sessionNotFound(req.SessionID)
	}
	if err != nil {
		return nil, orcaError(err)
	}
	id := core.WorkspaceOfEntry(tr.Workspace)
	release, ok := s.use(id)
	if !ok {
		return nil, core.Errorf(core.CodeInvalidState, "workspace is being removed")
	}
	defer release()
	if _, err := s.repo(ctx, id); err != nil {
		return nil, sessionNotFound(req.SessionID)
	}
	rec.WorkspaceID = id
	cursor := tr.Cursor
	if cursor == "" {
		cursor = req.Cursor
	}
	return &AgentMessages{Messages: tr.Messages, Cursor: cursor, Activity: tr.Activity, Terminal: tr.Terminal, Liveness: tr.Liveness}, nil
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
	a, release, err := s.useSession(ctx, req.SessionID)
	if err != nil {
		return nil, err
	}
	release()
	rec.WorkspaceID = a.Workspace
	return a, nil
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
		sess, release, err := s.useSession(ctx, req.SessionID)
		if err != nil {
			return nil, err
		}
		defer release()
		if err := s.orca.StopDispatch(ctx, req.SessionID); err != nil {
			slog.Warn("cannot stop the Dispatch of an agent session", "session", req.SessionID, "err", err)
		}
		if sess.Terminal != "" {
			if err := s.orca.CloseTerminal(ctx, sess.Terminal); err != nil {
				return nil, orcaError(err)
			}
		}
		return StopResult{SessionID: req.SessionID, Stopped: true}, nil
	})
}

// session reads session id from Orca. While its terminal is open, the
// terminal must run in a Reach workspace; once it is closed, the transcript
// must. Otherwise the session is not found.
func (s *Service) session(ctx context.Context, id string) (*core.AgentSession, error) {
	d, err := s.orca.Dispatch(ctx, id)
	if errors.Is(err, core.ErrNotFound) {
		return nil, sessionNotFound(id)
	}
	if err != nil {
		return nil, orcaError(err)
	}
	terms, err := s.orca.Terminals(ctx)
	if err != nil {
		return nil, orcaError(err)
	}
	sess := &core.AgentSession{ID: id, Liveness: d.Liveness, WaitingOnHuman: d.WaitingOnHuman}
	for _, t := range terms {
		if t.Handle == d.Terminal && t.Connected && d.Terminal != "" {
			sess.Workspace, sess.Terminal, sess.Agent = core.WorkspaceOfEntry(t.WorktreeID), t.Handle, t.AgentIdentity
		}
	}
	if sess.Workspace == "" {
		tr, err := s.orca.Transcript(ctx, id, "", 1)
		if err != nil {
			return nil, sessionNotFound(id)
		}
		sess.Workspace = core.WorkspaceOfEntry(tr.Workspace)
	}
	if _, err := s.repo(ctx, sess.Workspace); err != nil {
		return nil, sessionNotFound(id)
	}
	return sess, nil
}

// useSession reads session id like session and holds its workspace, as a
// request holding it, until release; it fails while cleanup removes the
// workspace.
func (s *Service) useSession(ctx context.Context, id string) (*core.AgentSession, func(), error) {
	sess, err := s.session(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	release, ok := s.use(sess.Workspace)
	if !ok {
		return nil, nil, core.Errorf(core.CodeInvalidState, "workspace is being removed")
	}
	return sess, release, nil
}

func sessionNotFound(id string) error {
	return core.Errorf(core.CodeNotFound, "agent session %q not found", id)
}
