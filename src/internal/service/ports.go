package service

import (
	"context"

	"github.com/takezoh/syntoria-reach/src/internal/core"
)

// The ports below are every side effect the service performs. Adapters
// implement them (store, workspace, command, job, agent, orca, ids); the
// composition root (cmd/reach) wires them and tests replace them with the
// fakes in servicetest.

// Store keeps Reach's records. Conditional updates are part of the
// contract: they decide races between concurrent requests and late reports.
type Store interface {
	// Admit records a new running operation. If the client already used
	// operationID, the stored record is returned with created=false and
	// nothing is written.
	Admit(clientID, operationID, kind, argsHash string) (op *core.Operation, created bool, err error)
	// Finish stores the outcome (result is stored as JSON) of a running
	// operation; an operation that is no longer running is left unchanged.
	// It returns the stored record.
	Finish(clientID, operationID, state string, result any, errCode, errMessage string) (*core.Operation, error)
	GetOperation(clientID, operationID string) (*core.Operation, error)
	Audit(core.AuditRecord) error

	// CreateWorkspace records w in the creating state.
	CreateWorkspace(w *core.Workspace) error
	// SetWorkspaceState moves a workspace from one state to another and
	// reports whether it was in the from state. Empty orcaID, path and branch
	// are left unchanged.
	SetWorkspaceState(id, from, to, orcaID, path, branch, errMessage string) (bool, error)
	GetWorkspace(clientID, id string) (*core.Workspace, error)
	// ListWorkspaces returns the client's ready workspaces, oldest first.
	ListWorkspaces(clientID string) ([]*core.Workspace, error)
	// WorkspaceBusy reports whether a workspace has running jobs or active
	// agent sessions.
	WorkspaceBusy(id string) (bool, error)

	// CreateJob records j as running.
	CreateJob(j *core.Job) error
	// FinishJob stores the outcome of a running job; a job that is no
	// longer running is left unchanged.
	FinishJob(id, state string, exitCode *int, timedOut, truncated bool, errMessage string) error
	GetJob(clientID, id string) (*core.Job, error)

	// CreateAgentSession records a in the starting state.
	CreateAgentSession(a *core.AgentSession) error
	// UpdateAgentSession applies u to a session that has not ended; closed,
	// failed and unknown sessions are never changed.
	UpdateAgentSession(id string, u core.AgentSessionUpdate) error
	GetAgentSession(clientID, id string) (*core.AgentSession, error)
	// AppendAgentEvent appends an event with the session's next sequence
	// number (starting at 1).
	AppendAgentEvent(sessionID string, turn int, typ, text string) error
	// ListAgentEvents returns up to limit events with seq > after, in order.
	ListAgentEvents(sessionID string, after int64, limit int) ([]core.AgentEvent, error)
}

// Files gives access to one workspace's files, confined to its root.
// Errors are core.ErrInvalidPath, ErrOutside, ErrNotFound, ErrNotRegular or
// ErrVersionConflict when they are the caller's to fix.
type Files interface {
	List(rel string, limit int) (entries []core.FileEntry, truncated bool, err error)
	Read(rel string, offset int64, limit int) (*core.FileChunk, error)
	// Write replaces the file if its version is expectedVersion ("" means it
	// must not exist) and returns the new version.
	Write(rel string, data []byte, expectedVersion string) (version string, err error)
	Close() error
}

// OpenFiles opens the workspace rooted at dir.
type OpenFiles func(dir string) (Files, error)

// RunCommand runs a command and waits for it. A non-zero exit or a timeout
// is a result; an error means the command could not be started.
type RunCommand func(ctx context.Context, s core.CommandSpec) (*core.CommandResult, error)

// Jobs runs background commands.
type Jobs interface {
	// Start starts job id; done is called once, later, with what happened.
	// An error means the job did not start and done is not called.
	Start(id string, s core.JobSpec, done func(core.JobExit)) error
	// Cancel kills a running job and returns after its done callback
	// returned; core.ErrJobNotRunning if it is not running.
	Cancel(id string) error
	ReadOutput(id, stream string, offset int64, limit int) (*core.JobOutput, error)
}

// Agents runs agent harness sessions.
type Agents interface {
	// Start launches session id and returns its protocol session ID. Once it
	// returned, what happens in the session is reported to events.
	Start(ctx context.Context, id string, s core.AgentStart, events core.AgentEvents) (protocolSessionID string, err error)
	// Prompt starts a turn and returns its number; core.ErrSessionInactive
	// or core.ErrSessionBusy when it cannot.
	Prompt(id, text string) (turn int, err error)
	// Stop ends the session and returns after its last report;
	// core.ErrSessionInactive if it is not running.
	Stop(id string) error
}

// Orca creates and removes workspaces (worktrees). Failures Orca reports
// are *core.OrcaError.
type Orca interface {
	CreateWorktree(ctx context.Context, repoPath, name string) (*core.Worktree, error)
	RemoveWorktree(ctx context.Context, id string) error
}

// NewID returns prefix followed by a unique random suffix.
type NewID func(prefix string) string

// Deps are the adapters the service uses.
type Deps struct {
	Store      Store
	OpenFiles  OpenFiles
	RunCommand RunCommand
	Jobs       Jobs
	Agents     Agents
	Orca       Orca
	NewID      NewID
}
