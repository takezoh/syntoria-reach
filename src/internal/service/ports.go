package service

import (
	"context"
	"time"

	"github.com/takezoh/syntoria-reach/src/internal/core"
)

// The ports below are every side effect the service performs. Adapters
// implement them (store, workspace, command, job, orca, folders, ids); the
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

	// WorkspaceBusy reports whether a workspace has running jobs.
	// Workspaces and agent sessions are Orca's; the store keeps neither.
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

// Orca owns the projects that are workspaces and everything about them.
// Failures Orca reports are *core.OrcaError.
type Orca interface {
	// AddFolder registers an existing folder as a folder project and
	// returns its repo ID.
	AddFolder(ctx context.Context, path string) (string, error)
	Repos(ctx context.Context) ([]core.Repo, error)
	// Repo returns one project; core.ErrNotFound if Orca has none.
	Repo(ctx context.Context, id string) (*core.Repo, error)
	// Worktrees lists the workspace entries of one project, or of every
	// project when repoID is empty; core.ErrNotFound for an unknown
	// project. A partial list is an error.
	Worktrees(ctx context.Context, repoID string) ([]core.Worktree, error)
	// SetWorktree changes a workspace entry's comment and workspace
	// status; an empty value leaves that one unchanged.
	SetWorktree(ctx context.Context, id, comment, status string) error
	// DeleteProject removes a project, with its saved details, from Orca;
	// its folder stays.
	DeleteProject(ctx context.Context, repoID string) error

	// CreateTerminal opens a terminal in workspace entry worktreeID, types
	// command into its shell, and returns its handle.
	CreateTerminal(ctx context.Context, worktreeID, title, command string) (string, error)
	Terminals(ctx context.Context) ([]core.Terminal, error)
	// ShowTerminal shows one terminal; core.ErrNotFound if Orca has no
	// terminal with that handle, as once it closed.
	ShowTerminal(ctx context.Context, handle string) (*core.TerminalStatus, error)
	// WaitIdle waits up to timeout for the agent in terminal handle to wait
	// for a prompt, and reports whether it does. Orca may instead see it
	// stopped at something only a person can answer: blocked says what.
	WaitIdle(ctx context.Context, handle string, timeout time.Duration) (idle bool, blocked string, err error)
	// AgentTurn reads the latest turn of the agent in the terminal whose
	// pane key is paneKey, as Orca's agent status hooks report it; nil when
	// Orca reports no agent there.
	AgentTurn(ctx context.Context, paneKey string) (*core.AgentTurn, error)
	// SendPrompt types and submits text to the agent in terminal handle and
	// returns the stages Orca observed (input_accepted, turn_started).
	SendPrompt(ctx context.Context, handle, text string) ([]string, error)
	CloseTerminal(ctx context.Context, handle string) error
}

// Folders creates and removes workspace folders.
type Folders interface {
	// Make creates the folder path, which must not exist yet, and its
	// parents.
	Make(path string) error
	// Remove removes the folder path and everything in it.
	Remove(path string) error
}

// NewID returns prefix followed by a unique random suffix.
type NewID func(prefix string) string

// Deps are the adapters the service uses.
type Deps struct {
	Store      Store
	OpenFiles  OpenFiles
	RunCommand RunCommand
	Jobs       Jobs
	Orca       Orca
	Folders    Folders
	NewID      NewID
}
