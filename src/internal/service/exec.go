package service

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/takezoh/syntoria-reach/src/internal/agent"
	"github.com/takezoh/syntoria-reach/src/internal/config"
	"github.com/takezoh/syntoria-reach/src/internal/job"
	"github.com/takezoh/syntoria-reach/src/internal/store"
)

// Limits for jobs and agent sessions.
const (
	defaultJobTimeout = time.Hour
	maxJobTimeout     = 24 * time.Hour
	maxPromptSize     = 1 << 20
	defaultEventLimit = 100
	maxEventLimit     = 1000
)

// StartJobRequest starts an allowed executable in the background in the
// workspace root. TimeoutSeconds 0 means one hour.
type StartJobRequest struct {
	OperationID    string   `json:"operation_id"`
	Workspace      string   `json:"workspace"`
	Executable     string   `json:"executable"`
	Args           []string `json:"args"`
	TimeoutSeconds int      `json:"timeout_seconds"`
}

// StartJob starts a job once per operation ID. The operation result is the job.
func (s *Service) StartJob(c *Client, req StartJobRequest) (*store.Operation, error) {
	rec := store.AuditRecord{ClientID: c.ID, Action: "start_job", WorkspaceID: req.Workspace, Target: req.Executable, OperationID: req.OperationID}
	w, err := s.authorize(c, req.Workspace, config.PermCommandRun)
	if err == nil && !slices.Contains(w.commands, req.Executable) {
		err = errorf(CodePermissionDenied, "executable %q is not allowed on workspace %q", req.Executable, req.Workspace)
	}
	timeout := time.Duration(req.TimeoutSeconds) * time.Second
	if req.TimeoutSeconds == 0 {
		timeout = defaultJobTimeout
	}
	if err == nil && (timeout <= 0 || timeout > maxJobTimeout) {
		err = errorf(CodeInvalidRequest, "timeout_seconds must be between 1 and %d", int(maxJobTimeout.Seconds()))
	}
	if err != nil {
		s.audit(rec, err)
		return nil, err
	}
	return s.operation(c, "start_job", req.OperationID, req, rec, func() (any, error) {
		j, err := s.jobs.Start(job.Spec{
			ClientID: c.ID, WorkspaceID: req.Workspace, Dir: w.root,
			Executable: req.Executable, Args: req.Args, Timeout: timeout,
		})
		if err != nil {
			return nil, internal(err)
		}
		return j, nil
	})
}

// JobRequest identifies a job.
type JobRequest struct {
	JobID string `json:"job_id"`
}

// GetJob returns one of the client's jobs.
func (s *Service) GetJob(c *Client, req JobRequest) (j *store.Job, err error) {
	defer func() { s.audit(store.AuditRecord{ClientID: c.ID, Action: "get_job", Target: req.JobID}, err) }()
	return s.job(c, req.JobID)
}

func (s *Service) job(c *Client, id string) (*store.Job, error) {
	j, err := s.store.GetJob(c.ID, id)
	if errors.Is(err, store.ErrNotFound) {
		return nil, errorf(CodeNotFound, "job %q not found", id)
	}
	if err != nil {
		return nil, internal(err)
	}
	return j, nil
}

// ReadJobOutputRequest reads part of a job's stdout or stderr.
type ReadJobOutputRequest struct {
	JobID  string `json:"job_id"`
	Stream string `json:"stream"`
	Offset int64  `json:"offset"`
	Limit  int    `json:"limit"`
}

// JobOutput is a chunk of job output. EOF is true once the job has ended and
// the chunk reaches the end of the stream.
type JobOutput struct {
	job.Output
	State string `json:"state"`
	EOF   bool   `json:"eof"`
}

// ReadJobOutput reads job output by offset.
func (s *Service) ReadJobOutput(c *Client, req ReadJobOutputRequest) (out *JobOutput, err error) {
	defer func() {
		s.audit(store.AuditRecord{ClientID: c.ID, Action: "read_job_output", Target: req.JobID}, err)
	}()
	if req.Limit == 0 {
		req.Limit = defaultReadSize
	}
	if req.Offset < 0 || req.Limit < 0 || req.Limit > maxReadSize || (req.Stream != "stdout" && req.Stream != "stderr") {
		return nil, errorf(CodeInvalidRequest, "stream must be stdout or stderr, offset >= 0 and limit between 1 and %d", maxReadSize)
	}
	j, err := s.job(c, req.JobID)
	if err != nil {
		return nil, err
	}
	o, err := s.jobs.ReadOutput(j.ID, req.Stream, req.Offset, req.Limit)
	if err != nil {
		return nil, internal(err)
	}
	ended := j.State != store.JobRunning
	return &JobOutput{Output: *o, State: j.State, EOF: ended && o.Offset+int64(len(o.Data)) >= o.Size}, nil
}

// CancelJobRequest cancels a running job.
type CancelJobRequest struct {
	OperationID string `json:"operation_id"`
	JobID       string `json:"job_id"`
}

// CancelJob kills a running job once per operation ID. The result is the job
// after it stopped; cancelling an ended job leaves it unchanged.
func (s *Service) CancelJob(c *Client, req CancelJobRequest) (*store.Operation, error) {
	rec := store.AuditRecord{ClientID: c.ID, Action: "cancel_job", Target: req.JobID, OperationID: req.OperationID}
	if _, err := s.job(c, req.JobID); err != nil {
		s.audit(rec, err)
		return nil, err
	}
	return s.operation(c, "cancel_job", req.OperationID, req, rec, func() (any, error) {
		if err := s.jobs.Cancel(req.JobID); err != nil && !errors.Is(err, job.ErrNotRunning) {
			return nil, internal(err)
		}
		return s.job(c, req.JobID)
	})
}

// StartAgentSessionRequest starts an agent harness in the workspace root.
// ResumeSessionID is one of the client's ended sessions (same workspace and
// agent) whose conversation the new session continues. Model and Effort are
// passed to Codex and Claude agents.
type StartAgentSessionRequest struct {
	OperationID     string `json:"operation_id"`
	Workspace       string `json:"workspace"`
	Agent           string `json:"agent"`
	ResumeSessionID string `json:"resume_session_id"`
	Model           string `json:"model"`
	Effort          string `json:"effort"`
}

// StartAgentSession starts a session once per operation ID. The result is
// the session, ready for prompts.
func (s *Service) StartAgentSession(ctx context.Context, c *Client, req StartAgentSessionRequest) (*store.Operation, error) {
	rec := store.AuditRecord{ClientID: c.ID, Action: "start_agent_session", WorkspaceID: req.Workspace, Target: req.Agent, OperationID: req.OperationID}
	w, err := s.authorize(c, req.Workspace, config.PermAgentRun)
	a, known := s.agents[req.Agent]
	if err == nil && (!known || !slices.Contains(w.agents, req.Agent)) {
		err = errorf(CodePermissionDenied, "agent %q is not allowed on workspace %q", req.Agent, req.Workspace)
	}
	if err == nil && a.Protocol == config.ProtocolACP && (req.Model != "" || req.Effort != "") {
		err = errorf(CodeInvalidRequest, "model and effort apply to codex and claude agents only")
	}
	resume := ""
	if err == nil && req.ResumeSessionID != "" {
		resume, err = s.resumable(c, req)
	}
	if err != nil {
		s.audit(rec, err)
		return nil, err
	}
	ctx = context.WithoutCancel(ctx)
	return s.operation(c, "start_agent_session", req.OperationID, req, rec, func() (any, error) {
		sess, err := s.sessions.Start(ctx, agent.StartSpec{
			ClientID: c.ID, WorkspaceID: req.Workspace, Dir: w.root, Agent: a,
			Resume: resume, Model: req.Model, Effort: req.Effort,
		})
		if err != nil {
			return nil, errorf(CodeHarnessFailed, "%v", err)
		}
		return sess, nil
	})
}

// resumable returns the protocol session ID of a session the client may
// resume: its own, in the same workspace and agent, and no longer running.
// Protocol IDs are never taken from the request, so a client cannot attach
// to another client's conversation.
func (s *Service) resumable(c *Client, req StartAgentSessionRequest) (string, error) {
	prev, err := s.agentSession(c, req.ResumeSessionID)
	if err != nil {
		return "", err
	}
	switch {
	case prev.WorkspaceID != req.Workspace || prev.AgentID != req.Agent:
		return "", errorf(CodeInvalidRequest, "session %q belongs to another workspace or agent", prev.ID)
	case prev.State == store.SessionStarting || prev.State == store.SessionIdle || prev.State == store.SessionRunning:
		return "", errorf(CodeInvalidState, "session %q is still active; stop it first", prev.ID)
	case prev.ProtocolSessionID == "":
		return "", errorf(CodeInvalidState, "session %q never opened a protocol session", prev.ID)
	}
	return prev.ProtocolSessionID, nil
}

// SendAgentPromptRequest sends one user turn to a session.
type SendAgentPromptRequest struct {
	OperationID string `json:"operation_id"`
	SessionID   string `json:"session_id"`
	Text        string `json:"text"`
}

// PromptResult identifies the started turn.
type PromptResult struct {
	Turn int `json:"turn"`
}

// SendAgentPrompt starts a turn once per operation ID and returns without
// waiting for the agent. Progress is read with ReadAgentEvents.
func (s *Service) SendAgentPrompt(c *Client, req SendAgentPromptRequest) (*store.Operation, error) {
	rec := store.AuditRecord{ClientID: c.ID, Action: "send_agent_prompt", Target: req.SessionID, OperationID: req.OperationID}
	_, err := s.agentSession(c, req.SessionID)
	if err == nil && (req.Text == "" || len(req.Text) > maxPromptSize) {
		err = errorf(CodeInvalidRequest, "text must be 1 to %d bytes", maxPromptSize)
	}
	if err != nil {
		s.audit(rec, err)
		return nil, err
	}
	return s.operation(c, "send_agent_prompt", req.OperationID, req, rec, func() (any, error) {
		turn, err := s.sessions.Prompt(req.SessionID, req.Text)
		if err != nil {
			return nil, sessionError(err)
		}
		return PromptResult{Turn: turn}, nil
	})
}

// AgentSessionRequest identifies a session.
type AgentSessionRequest struct {
	SessionID string `json:"session_id"`
}

// GetAgentSession returns one of the client's sessions.
func (s *Service) GetAgentSession(c *Client, req AgentSessionRequest) (a *store.AgentSession, err error) {
	defer func() {
		s.audit(store.AuditRecord{ClientID: c.ID, Action: "get_agent_session", Target: req.SessionID}, err)
	}()
	return s.agentSession(c, req.SessionID)
}

func (s *Service) agentSession(c *Client, id string) (*store.AgentSession, error) {
	a, err := s.store.GetAgentSession(c.ID, id)
	if errors.Is(err, store.ErrNotFound) {
		return nil, errorf(CodeNotFound, "agent session %q not found", id)
	}
	if err != nil {
		return nil, internal(err)
	}
	return a, nil
}

// ReadAgentEventsRequest lists events after the After sequence number.
type ReadAgentEventsRequest struct {
	SessionID string `json:"session_id"`
	After     int64  `json:"after"`
	Limit     int    `json:"limit"`
}

// AgentEvents is a page of events plus the session's current state.
type AgentEvents struct {
	Events []store.AgentEvent `json:"events"`
	State  string             `json:"state"`
}

// ReadAgentEvents returns session events in order.
func (s *Service) ReadAgentEvents(c *Client, req ReadAgentEventsRequest) (out *AgentEvents, err error) {
	defer func() {
		s.audit(store.AuditRecord{ClientID: c.ID, Action: "read_agent_events", Target: req.SessionID}, err)
	}()
	if req.Limit == 0 {
		req.Limit = defaultEventLimit
	}
	if req.After < 0 || req.Limit < 0 || req.Limit > maxEventLimit {
		return nil, errorf(CodeInvalidRequest, "after must be >= 0 and limit between 1 and %d", maxEventLimit)
	}
	// Read the state first: events written before an ended state are all visible.
	a, err := s.agentSession(c, req.SessionID)
	if err != nil {
		return nil, err
	}
	events, err := s.store.ListAgentEvents(a.ID, req.After, req.Limit)
	if err != nil {
		return nil, internal(err)
	}
	return &AgentEvents{Events: events, State: a.State}, nil
}

// StopAgentSessionRequest ends a session.
type StopAgentSessionRequest struct {
	OperationID string `json:"operation_id"`
	SessionID   string `json:"session_id"`
}

// StopAgentSession interrupts any turn and ends the harness once per
// operation ID. The result is the session after it stopped; stopping a
// session that already ended leaves it unchanged.
func (s *Service) StopAgentSession(c *Client, req StopAgentSessionRequest) (*store.Operation, error) {
	rec := store.AuditRecord{ClientID: c.ID, Action: "stop_agent_session", Target: req.SessionID, OperationID: req.OperationID}
	if _, err := s.agentSession(c, req.SessionID); err != nil {
		s.audit(rec, err)
		return nil, err
	}
	return s.operation(c, "stop_agent_session", req.OperationID, req, rec, func() (any, error) {
		if err := s.sessions.Stop(req.SessionID); err != nil && !errors.Is(err, agent.ErrNotActive) {
			return nil, internal(err)
		}
		return s.agentSession(c, req.SessionID)
	})
}

func sessionError(err error) error {
	switch {
	case errors.Is(err, agent.ErrNotActive):
		return errorf(CodeInvalidState, "agent session is not active")
	case errors.Is(err, agent.ErrBusy):
		return errorf(CodeInvalidState, "a turn is already in progress")
	}
	return internal(err)
}
