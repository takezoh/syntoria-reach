package service

import (
	"context"
	"errors"
	"log/slog"
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
