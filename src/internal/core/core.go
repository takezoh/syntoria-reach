// Package core holds Reach's domain: the records it keeps, their states, the
// error classes it reports and the pure rules that decide what may happen.
// It has no side effects and depends only on the standard library; adapters
// (store, workspace, command, job, agent, orca) and the service share these
// types.
package core

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Permissions a client can be granted on a project. workspace.create lets
// the client create (and delete) its own workspaces from the project; the
// other permissions apply to those workspaces.
const (
	PermWorkspaceCreate = "workspace.create"
	PermFileRead        = "file.read"
	PermFileWrite       = "file.write"
	PermCommandRun      = "command.run" // run_command and jobs
	PermAgentRun        = "agent.run"
)

// Error classes returned to callers.
const (
	CodeInvalidRequest    = "invalid_request"
	CodeUnauthorized      = "unauthorized"
	CodePermissionDenied  = "permission_denied"
	CodeNotFound          = "not_found"
	CodeOperationConflict = "operation_conflict"
	CodeVersionConflict   = "version_conflict"
	CodeInvalidState      = "invalid_state"
	CodeHarnessFailed     = "harness_unavailable"
	CodeOrcaFailed        = "orca_failed"
	CodeResultUnknown     = "result_unknown"
	CodeInternal          = "internal"
)

// Error is a classified failure reported to a caller.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

// Errorf returns an Error with a formatted message.
func Errorf(code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// Errors reported by adapters and mapped to error classes by the service.
var (
	ErrNotFound        = errors.New("not found")
	ErrInvalidPath     = errors.New("invalid path")
	ErrOutside         = errors.New("path leaves the workspace")
	ErrVersionConflict = errors.New("version conflict")
	ErrNotRegular      = errors.New("unexpected file type")
	ErrJobNotRunning   = errors.New("job is not running")
	ErrSessionInactive = errors.New("agent session is not active")
	ErrSessionBusy     = errors.New("a turn is already in progress")
)

// Operation states. StateUnknown marks an operation whose outcome could not
// be confirmed, e.g. the daemon stopped while it was running; it is never
// re-executed.
const (
	StateRunning   = "running"
	StateSucceeded = "succeeded"
	StateFailed    = "failed"
	StateUnknown   = "unknown"
)

// Operation is the durable record of one side-effecting request.
type Operation struct {
	ClientID     string          `json:"-"`
	OperationID  string          `json:"operation_id"`
	Kind         string          `json:"kind"`
	ArgsHash     string          `json:"-"`
	State        string          `json:"state"`
	Result       json.RawMessage `json:"result,omitempty"`
	ErrorCode    string          `json:"error_code,omitempty"`
	ErrorMessage string          `json:"error_message,omitempty"`
	CreatedAt    time.Time       `json:"created_at"`
	UpdatedAt    time.Time       `json:"updated_at"`
}

// AuditRecord describes one request. It never contains file contents,
// command output, or environment values.
type AuditRecord struct {
	ClientID    string
	Action      string
	WorkspaceID string
	Target      string
	OperationID string
	Outcome     string
	ErrorCode   string
}

// WorkspacePrefix starts the name of every Orca project Reach creates as a
// workspace. Reach sees only projects whose name has it.
const WorkspacePrefix = "reach-"

// CleanupAfter is how long a completed workspace stays after its last
// activity before Reach removes it.
const CleanupAfter = 7 * 24 * time.Hour

// Repo is a project registered in Orca, as Orca reports it.
type Repo struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName"`
	Path        string `json:"path"`
	Kind        string `json:"kind"`
}

// Worktree is a workspace entry of an Orca project, as Orca reports it. A
// project's root entry (IsMainWorktree) carries its comment and workspace
// status.
type Worktree struct {
	ID              string `json:"id"`
	RepoID          string `json:"repoId"`
	Path            string `json:"path"`
	Comment         string `json:"comment"`
	WorkspaceStatus string `json:"workspaceStatus"`
	IsMainWorktree  bool   `json:"isMainWorktree"`
	LastActivityAt  int64  `json:"lastActivityAt"` // Unix milliseconds
}

// Workspace is a Reach workspace: one unit of work, kept by Orca as a project
// whose name has WorkspacePrefix. Its ID is the Orca project's repo ID, its
// note the project's comment and its status the project's Orca workspace
// status (todo, in-progress, in-review, completed or a custom one). Reach
// keeps nothing about it and reads it from Orca on every request.
type Workspace struct {
	ID     string `json:"workspace"`
	Name   string `json:"name"`
	Note   string `json:"note"`
	Status string `json:"status"`
	Path   string `json:"path"`
}

// OrcaError is a failure reported by Orca.
type OrcaError struct {
	Code    string
	Message string
}

func (e *OrcaError) Error() string { return "orca: " + e.Code + ": " + e.Message }

// FileEntry is one item of a directory listing.
type FileEntry struct {
	Name    string    `json:"name"`
	Type    string    `json:"type"` // file, dir, symlink, other
	Size    int64     `json:"size"`
	ModTime time.Time `json:"mod_time"`
}

// FileChunk is a bounded slice of a file plus the file's version.
type FileChunk struct {
	Data    []byte `json:"data"`
	Size    int64  `json:"size"`
	Offset  int64  `json:"offset"`
	EOF     bool   `json:"eof"`
	Version string `json:"version"`
}

// CommandSpec describes one synchronous execution. Executable is an absolute
// path and Args are passed verbatim (no shell).
type CommandSpec struct {
	Executable  string
	Args        []string
	Dir         string
	Timeout     time.Duration
	OutputLimit int // per stream, in bytes
}

// CommandResult is the observed outcome of a finished command.
type CommandResult struct {
	ExitCode   int    `json:"exit_code"`
	Stdout     []byte `json:"stdout"`
	Stderr     []byte `json:"stderr"`
	Truncated  bool   `json:"truncated"`
	TimedOut   bool   `json:"timed_out"`
	DurationMS int64  `json:"duration_ms"`
}

// Job states. Running jobs left by a previous daemon become StateUnknown.
const (
	JobRunning   = "running"
	JobSucceeded = "succeeded"
	JobFailed    = "failed"
	JobCancelled = "cancelled"
)

// Job is the durable record of a background command.
type Job struct {
	ID           string    `json:"job_id"`
	ClientID     string    `json:"-"`
	WorkspaceID  string    `json:"workspace"`
	Executable   string    `json:"executable"`
	State        string    `json:"state"`
	ExitCode     *int      `json:"exit_code,omitempty"`
	TimedOut     bool      `json:"timed_out"`
	Truncated    bool      `json:"truncated"`
	ErrorMessage string    `json:"error_message,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// JobSpec describes a background command.
type JobSpec struct {
	Dir        string
	Executable string
	Args       []string
	Timeout    time.Duration
}

// JobExit is what happened to a background command. ExitCode is nil when the
// process was killed by a signal.
type JobExit struct {
	ExitCode  *int
	TimedOut  bool
	Cancelled bool
	Truncated bool
	Err       string // failure to wait for the process, if any
}

// JobOutput is a chunk of a job's stdout or stderr.
type JobOutput struct {
	Data   []byte `json:"data"`
	Offset int64  `json:"offset"`
	Size   int64  `json:"size"`
}

// Agent harness protocols.
const (
	ProtocolCodex  = "codex"  // Codex app-server
	ProtocolClaude = "claude" // Claude Code stream-json
	ProtocolACP    = "acp"    // Agent Client Protocol v1 (e.g. DSH)
)

// Harness is how to start an agent. Command[0] is an absolute path; Env is
// added to the minimal child environment. For Claude, Reach appends the
// stream-json and session flags; permission mode and sandbox flags belong in
// Command. Sandbox is the Codex sandbox mode; AllowPermissions lets Reach
// approve ACP permission requests.
type Harness struct {
	Protocol         string
	Command          []string
	Env              map[string]string
	Sandbox          string
	AllowPermissions bool
}

// AgentStart describes a session to start. Resume is a protocol session ID
// to continue; Model and Effort apply to Codex and Claude.
type AgentStart struct {
	Dir     string
	Harness Harness
	Resume  string
	Model   string
	Effort  string
}

// AgentEvents receives what happens in a running agent session. The service
// implements it and records the facts. Calls for one session never overlap.
type AgentEvents interface {
	// TurnStarted is called before a prompt is delivered; an error aborts it.
	TurnStarted(sessionID string, turn int) error
	// Event reports a normalized event (EventMessage, EventError,
	// EventTurnCompleted) of a turn.
	Event(sessionID string, turn int, typ, text string)
	// TurnEnded is called when a turn finished and the harness is still up.
	TurnEnded(sessionID string, turn int)
	// Exited is called when the harness ended without being stopped; turn is
	// the last turn started (0 if none).
	Exited(sessionID string, turn int)
}

// Agent session states. Active sessions left by a previous daemon become
// StateUnknown.
const (
	SessionStarting = "starting"
	SessionIdle     = "idle"
	SessionRunning  = "running"
	SessionClosed   = "closed"
	SessionFailed   = "failed"
)

// Event types recorded for a session.
const (
	EventMessage       = "message"        // text the agent sent to the user
	EventError         = "error"          // an error reported by the harness or Reach
	EventTurnCompleted = "turn_completed" // text is the stop reason / turn status
)

// AgentSession is the durable record of an agent harness session.
type AgentSession struct {
	ID                string    `json:"session_id"`
	ClientID          string    `json:"-"`
	WorkspaceID       string    `json:"workspace"`
	AgentID           string    `json:"agent"`
	ProtocolSessionID string    `json:"protocol_session_id,omitempty"`
	State             string    `json:"state"`
	Turns             int       `json:"turns"`
	ErrorMessage      string    `json:"error_message,omitempty"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
}

// AgentSessionUpdate changes a session; empty fields are left unchanged.
type AgentSessionUpdate struct {
	State             string
	ProtocolSessionID string
	ErrorMessage      string
	AddTurn           bool
}

// AgentEvent is one normalized event of an agent session.
type AgentEvent struct {
	Seq  int64     `json:"seq"`
	Turn int       `json:"turn"`
	Type string    `json:"type"`
	Text string    `json:"text"`
	At   time.Time `json:"at"`
}
