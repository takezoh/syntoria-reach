package service

import (
	"context"
	"time"

	"github.com/takezoh/syntoria-reach/src/internal/core"
)

// The ports below are every side effect the service performs. Adapters
// implement them (store, workspace, command, job, the workspace backends
// orcaws and localws, ids); the composition root (cmd/reach) wires them and
// tests replace them with the fakes in servicetest.

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

	// WorkspaceBusy reports whether a workspace has running jobs.
	// Workspaces and agent sessions are the workspace backend's.
	WorkspaceBusy(id string) (bool, error)
	// WorkspaceLastUsed returns when Reach last acted on a workspace (its
	// audit records and jobs), or the zero time.
	WorkspaceLastUsed(id string) (time.Time, error)

	// CreateJob records j as running.
	CreateJob(j *core.Job) error
	// FinishJob stores the outcome of a running job; a job that is no
	// longer running is left unchanged.
	FinishJob(id, state string, exitCode *int, timedOut, truncated bool, errMessage string) error
	GetJob(clientID, id string) (*core.Job, error)
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

// Workspaces keeps the node's workspaces and runs agents in them. Two
// backends implement it: orcaws keeps workspaces as projects of the Orca
// server and runs agents in its terminals; localws keeps them as folders it
// records itself and runs agents as processes of the daemon.
//
// Failures the caller is to see are *core.Error (orca_failed,
// harness_unavailable, invalid_state, invalid_request); a workspace or
// session that does not exist is core.ErrNotFound. Other errors are
// internal.
type Workspaces interface {
	// Create makes workspace name (which has core.WorkspacePrefix) with
	// note: a new, empty folder of that name under the workspaces folder.
	Create(ctx context.Context, name, note string) (*core.Workspace, error)
	List(ctx context.Context) ([]*core.Workspace, error)
	Get(ctx context.Context, id string) (*core.Workspace, error)
	// Update changes a workspace's note and status; an empty value leaves
	// that one unchanged.
	Update(ctx context.Context, id, note, status string) (*core.Workspace, error)
	// Remove removes workspace w, and its folder if Reach created it.
	Remove(ctx context.Context, w *core.Workspace) error
	// Sweep removes the workspace folders Reach created that no workspace
	// has any more, such as those of projects removed in Orca, once nothing
	// in them changed since; it returns the names of the folders removed.
	Sweep(ctx context.Context, since time.Time) ([]string, error)
	// Busy returns the workspaces in which a session is open.
	Busy(ctx context.Context) (map[string]bool, error)

	// StartAgent starts agent a in workspace w and returns the session once
	// the agent waits for its first prompt, or after a while; an agent that
	// does not start is harness_unavailable, and leaves no session behind.
	StartAgent(ctx context.Context, w *core.Workspace, a core.Agent) (*core.AgentSession, error)
	// Session reads session id with what its agent does, and its reply
	// once it waits for a prompt.
	Session(ctx context.Context, id string) (*core.AgentStatus, error)
	// Prompt delivers text to the agent of session id, which Session found
	// waiting for a prompt, and returns once the agent started its turn,
	// with the stages observed; an agent that did not start it is
	// harness_unavailable.
	Prompt(ctx context.Context, id, text string) ([]string, error)
	// Stop ends session id and its agent.
	Stop(ctx context.Context, id string) error
}

// NewID returns prefix followed by a unique random suffix.
type NewID func(prefix string) string

// Deps are the adapters the service uses.
type Deps struct {
	Store      Store
	OpenFiles  OpenFiles
	RunCommand RunCommand
	Jobs       Jobs
	Workspaces Workspaces
	NewID      NewID
}
