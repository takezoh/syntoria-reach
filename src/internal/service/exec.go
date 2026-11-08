package service

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"time"

	"github.com/takezoh/syntoria-reach/src/internal/core"
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

// StartJob starts a job once per operation ID. The operation result is the
// job; a job that could not start is a failed job.
func (s *Service) StartJob(ctx context.Context, c *Client, req StartJobRequest) (*core.Operation, error) {
	rec := core.AuditRecord{ClientID: c.ID, Action: "start_job", WorkspaceID: req.Workspace, Target: req.Executable, OperationID: req.OperationID}
	w, timeout, err := s.authorizeExecutable(ctx, c, req.Workspace, req.Executable, req.TimeoutSeconds, defaultJobTimeout, maxJobTimeout)
	if err != nil {
		s.audit(rec, err)
		return nil, err
	}
	defer w.release() // until the job is recorded
	return s.operation(c, "start_job", req.OperationID, req, rec, func() (any, error) {
		id := s.newID("job_")
		if err := s.store.CreateJob(&core.Job{ID: id, ClientID: c.ID, WorkspaceID: req.Workspace, Executable: req.Executable}); err != nil {
			return nil, internal(err)
		}
		spec := core.JobSpec{Dir: w.root, Executable: req.Executable, Args: req.Args, Timeout: timeout}
		if err := s.jobs.Start(id, spec, func(e core.JobExit) { s.finishJob(id, e) }); err != nil {
			if err := s.store.FinishJob(id, core.JobFailed, nil, false, false, err.Error()); err != nil {
				return nil, internal(err)
			}
		}
		return s.job(c, id)
	})
}

// finishJob records what happened to a job. If it cannot, the job stays
// running in the store and becomes unknown on restart.
func (s *Service) finishJob(id string, e core.JobExit) {
	state, msg := core.JobOutcome(e)
	if err := s.store.FinishJob(id, state, e.ExitCode, e.TimedOut, e.Truncated, msg); err != nil {
		slog.Error("record job outcome", "job", id, "err", err)
	}
}

// JobRequest identifies a job.
type JobRequest struct {
	JobID string `json:"job_id"`
}

// GetJob returns one of the client's jobs.
func (s *Service) GetJob(c *Client, req JobRequest) (j *core.Job, err error) {
	defer func() { s.audit(core.AuditRecord{ClientID: c.ID, Action: "get_job", Target: req.JobID}, err) }()
	return s.job(c, req.JobID)
}

func (s *Service) job(c *Client, id string) (*core.Job, error) {
	j, err := s.store.GetJob(c.ID, id)
	if errors.Is(err, core.ErrNotFound) {
		return nil, core.Errorf(core.CodeNotFound, "job %q not found", id)
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
	core.JobOutput
	State string `json:"state"`
	EOF   bool   `json:"eof"`
}

// ReadJobOutput reads job output by offset.
func (s *Service) ReadJobOutput(c *Client, req ReadJobOutputRequest) (out *JobOutput, err error) {
	defer func() {
		s.audit(core.AuditRecord{ClientID: c.ID, Action: "read_job_output", Target: req.JobID}, err)
	}()
	if req.Limit == 0 {
		req.Limit = defaultReadSize
	}
	if req.Offset < 0 || req.Limit < 0 || req.Limit > maxReadSize || (req.Stream != "stdout" && req.Stream != "stderr") {
		return nil, core.Errorf(core.CodeInvalidRequest, "stream must be stdout or stderr, offset >= 0 and limit between 1 and %d", maxReadSize)
	}
	// Read the state first: output written before an ended state is all visible.
	j, err := s.job(c, req.JobID)
	if err != nil {
		return nil, err
	}
	o, err := s.jobs.ReadOutput(j.ID, req.Stream, req.Offset, req.Limit)
	if err != nil {
		return nil, internal(err)
	}
	ended := j.State != core.JobRunning
	return &JobOutput{JobOutput: *o, State: j.State, EOF: ended && o.Offset+int64(len(o.Data)) >= o.Size}, nil
}

// CancelJobRequest cancels a running job.
type CancelJobRequest struct {
	OperationID string `json:"operation_id"`
	JobID       string `json:"job_id"`
}

// CancelJob kills a running job once per operation ID. The result is the job
// after it stopped; cancelling an ended job leaves it unchanged.
func (s *Service) CancelJob(c *Client, req CancelJobRequest) (*core.Operation, error) {
	rec := core.AuditRecord{ClientID: c.ID, Action: "cancel_job", Target: req.JobID, OperationID: req.OperationID}
	if _, err := s.job(c, req.JobID); err != nil {
		s.audit(rec, err)
		return nil, err
	}
	return s.operation(c, "cancel_job", req.OperationID, req, rec, func() (any, error) {
		if err := s.jobs.Cancel(req.JobID); err != nil && !errors.Is(err, core.ErrJobNotRunning) {
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
func (s *Service) StartAgentSession(ctx context.Context, c *Client, req StartAgentSessionRequest) (*core.Operation, error) {
	rec := core.AuditRecord{ClientID: c.ID, Action: "start_agent_session", WorkspaceID: req.Workspace, Target: req.Agent, OperationID: req.OperationID}
	w, err := s.authorize(ctx, c, req.Workspace, core.PermAgentRun)
	if err == nil {
		defer w.release() // until the session is recorded
	}
	a, known := s.harness[req.Agent]
	if err == nil && (!known || !slices.Contains(w.agents, req.Agent)) {
		err = core.Errorf(core.CodePermissionDenied, "agent %q is not allowed on workspace %q", req.Agent, req.Workspace)
	}
	if err == nil && a.Protocol == core.ProtocolACP && (req.Model != "" || req.Effort != "") {
		err = core.Errorf(core.CodeInvalidRequest, "model and effort apply to codex and claude agents only")
	}
	resume := ""
	if err == nil && req.ResumeSessionID != "" {
		var prev *core.AgentSession
		if prev, err = s.agentSession(c, req.ResumeSessionID); err == nil {
			resume, err = core.Resumable(prev, req.Workspace, req.Agent)
		}
	}
	if err != nil {
		s.audit(rec, err)
		return nil, err
	}
	ctx = context.WithoutCancel(ctx)
	return s.operation(c, "start_agent_session", req.OperationID, req, rec, func() (any, error) {
		id := s.newID("as_")
		if err := s.store.CreateAgentSession(&core.AgentSession{ID: id, ClientID: c.ID, WorkspaceID: req.Workspace, AgentID: req.Agent}); err != nil {
			return nil, internal(err)
		}
		start := core.AgentStart{Dir: w.root, Harness: a.Harness(), Resume: resume, Model: req.Model, Effort: req.Effort}
		protoID, err := s.agents.Start(ctx, id, start, sessionRecorder{s.store})
		if err != nil {
			if uerr := s.store.UpdateAgentSession(id, core.AgentSessionUpdate{State: core.SessionFailed, ErrorMessage: err.Error()}); uerr != nil {
				return nil, internal(uerr)
			}
			return nil, core.Errorf(core.CodeHarnessFailed, "%v", err)
		}
		// A harness that already exited left the session failed; ended
		// sessions are never changed.
		if err := s.store.UpdateAgentSession(id, core.AgentSessionUpdate{State: core.SessionIdle, ProtocolSessionID: protoID}); err != nil {
			s.agents.Stop(id)
			return nil, internal(err)
		}
		s.reopen(ctx, req.Workspace)
		return s.agentSession(c, id)
	})
}

// sessionRecorder records what the agent runtime reports about a session.
type sessionRecorder struct{ store Store }

func (r sessionRecorder) TurnStarted(id string, turn int) error {
	return r.store.UpdateAgentSession(id, core.AgentSessionUpdate{State: core.SessionRunning, AddTurn: true})
}

func (r sessionRecorder) Event(id string, turn int, typ, text string) {
	if err := r.store.AppendAgentEvent(id, turn, typ, text); err != nil {
		slog.Error("record agent event", "session", id, "err", err)
	}
}

func (r sessionRecorder) TurnEnded(id string, turn int) {
	if err := r.store.UpdateAgentSession(id, core.AgentSessionUpdate{State: core.SessionIdle}); err != nil {
		slog.Error("record agent turn end", "session", id, "err", err)
	}
}

func (r sessionRecorder) Exited(id string, turn int) {
	const msg = "harness exited"
	r.Event(id, turn, core.EventError, msg)
	if err := r.store.UpdateAgentSession(id, core.AgentSessionUpdate{State: core.SessionFailed, ErrorMessage: msg}); err != nil {
		slog.Error("record agent exit", "session", id, "err", err)
	}
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
func (s *Service) SendAgentPrompt(c *Client, req SendAgentPromptRequest) (*core.Operation, error) {
	rec := core.AuditRecord{ClientID: c.ID, Action: "send_agent_prompt", Target: req.SessionID, OperationID: req.OperationID}
	_, err := s.agentSession(c, req.SessionID)
	if err == nil && (req.Text == "" || len(req.Text) > maxPromptSize) {
		err = core.Errorf(core.CodeInvalidRequest, "text must be 1 to %d bytes", maxPromptSize)
	}
	if err != nil {
		s.audit(rec, err)
		return nil, err
	}
	return s.operation(c, "send_agent_prompt", req.OperationID, req, rec, func() (any, error) {
		turn, err := s.agents.Prompt(req.SessionID, req.Text)
		if err != nil {
			return nil, classify(err)
		}
		return PromptResult{Turn: turn}, nil
	})
}

// AgentSessionRequest identifies a session.
type AgentSessionRequest struct {
	SessionID string `json:"session_id"`
}

// GetAgentSession returns one of the client's sessions.
func (s *Service) GetAgentSession(c *Client, req AgentSessionRequest) (a *core.AgentSession, err error) {
	defer func() {
		s.audit(core.AuditRecord{ClientID: c.ID, Action: "get_agent_session", Target: req.SessionID}, err)
	}()
	return s.agentSession(c, req.SessionID)
}

func (s *Service) agentSession(c *Client, id string) (*core.AgentSession, error) {
	a, err := s.store.GetAgentSession(c.ID, id)
	if errors.Is(err, core.ErrNotFound) {
		return nil, core.Errorf(core.CodeNotFound, "agent session %q not found", id)
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
	Events []core.AgentEvent `json:"events"`
	State  string            `json:"state"`
}

// ReadAgentEvents returns session events in order.
func (s *Service) ReadAgentEvents(c *Client, req ReadAgentEventsRequest) (out *AgentEvents, err error) {
	defer func() {
		s.audit(core.AuditRecord{ClientID: c.ID, Action: "read_agent_events", Target: req.SessionID}, err)
	}()
	if req.Limit == 0 {
		req.Limit = defaultEventLimit
	}
	if req.After < 0 || req.Limit < 0 || req.Limit > maxEventLimit {
		return nil, core.Errorf(core.CodeInvalidRequest, "after must be >= 0 and limit between 1 and %d", maxEventLimit)
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
func (s *Service) StopAgentSession(c *Client, req StopAgentSessionRequest) (*core.Operation, error) {
	rec := core.AuditRecord{ClientID: c.ID, Action: "stop_agent_session", Target: req.SessionID, OperationID: req.OperationID}
	if _, err := s.agentSession(c, req.SessionID); err != nil {
		s.audit(rec, err)
		return nil, err
	}
	return s.operation(c, "stop_agent_session", req.OperationID, req, rec, func() (any, error) {
		err := s.agents.Stop(req.SessionID)
		switch {
		case errors.Is(err, core.ErrSessionInactive):
		case err != nil:
			return nil, internal(err)
		default:
			if err := s.store.UpdateAgentSession(req.SessionID, core.AgentSessionUpdate{State: core.SessionClosed}); err != nil {
				return nil, internal(err)
			}
		}
		return s.agentSession(c, req.SessionID)
	})
}
