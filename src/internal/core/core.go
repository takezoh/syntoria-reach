// Package core holds Reach's domain: the records it keeps, their states, the
// error classes it reports and the pure rules that decide what may happen.
// It has no side effects and depends only on the standard library; adapters
// (store, workspace, command, job, agent, orca and the workspace backends)
// and the service share these types.
package core

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Permissions a client can be granted. They apply to every workspace;
// workspace.create lets the client create and update workspaces.
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

// Workspace is a Reach workspace: one unit of work, a folder of its own
// named with WorkspacePrefix, with a note saying what the work is and a
// status (todo, in-progress, in-review, completed, or on Orca a custom one).
// The node's workspace backend keeps it: Orca as a project (the ID is the
// project's repo ID, the note and status its root entry's comment and
// workspace status), or Reach itself as a record of a local workspace.
// LastActivity is when the backend last saw activity in it.
type Workspace struct {
	ID           string    `json:"workspace"`
	Name         string    `json:"name"`
	Note         string    `json:"note"`
	Status       string    `json:"status"`
	Path         string    `json:"path"`
	LastActivity time.Time `json:"-"`
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

// Terminal is an Orca terminal, as Orca reports it. WorktreeID is the ID
// of the workspace entry it runs in; AgentIdentity names the agent Orca
// recognizes running in it, empty when there is none (a shell). TabID and
// LeafID place it in Orca's layout.
type Terminal struct {
	Handle        string `json:"handle"`
	WorktreeID    string `json:"worktreeId"`
	Connected     bool   `json:"connected"`
	AgentIdentity string `json:"agentIdentity"`
	TabID         string `json:"tabId,omitempty"`
	LeafID        string `json:"leafId,omitempty"`
}

// PaneKey is the key under which Orca reports the agent in the terminal
// (<tabId>:<leafId>), or empty when Orca did not place the terminal.
func (t Terminal) PaneKey() string {
	if t.TabID == "" || t.LeafID == "" {
		return ""
	}
	return t.TabID + ":" + t.LeafID
}

// TerminalStatus is one terminal as Orca shows it. WaitingOnHuman is what
// Orca saw the agent in it wait for from its user, such as a permission:
// empty when Orca looked and saw nothing, WaitUnknown when it did not look.
type TerminalStatus struct {
	Terminal
	WaitingOnHuman string
}

// WaitUnknown is TerminalStatus.WaitingOnHuman when Orca could not tell
// whether the agent waits for its user.
const WaitUnknown = "unknown"

// AgentTurn is the latest turn of an agent as Orca's agent status hooks
// report it: whether the agent is still in it, and once it ended, the
// agent's last message, which Orca keeps only up to a limit; Truncated says
// the message reached that limit and may be cut.
type AgentTurn struct {
	Running   bool
	Reply     string
	Truncated bool
}

// What an agent in a session does, as Orca observes it.
const (
	AgentIdle           = "idle"             // waits for a prompt
	AgentWorking        = "working"          // in a turn, or not yet ready
	AgentWaitingOnHuman = "waiting_on_human" // asks its user something, in Orca
	AgentExited         = "exited"           // no agent runs in the terminal: a shell
)

// AgentSession is an agent Reach started in a workspace: in an Orca
// terminal, whose handle is the session ID and which ends the session when
// it closes, or as a process of the daemon in a local workspace.
type AgentSession struct {
	ID             string `json:"session_id"`
	Workspace      string `json:"workspace"`
	Agent          string `json:"agent,omitempty"` // the agent running in it, if any
	State          string `json:"state"`
	WaitingOnHuman string `json:"waiting_on_human,omitempty"`
}

// AgentStatus is a session as its workspace backend observes it and, once
// the agent waits for a prompt (State is AgentIdle), its last message of
// the turn: its reply to the last prompt, empty when there is none.
// ReplyTruncated says the reply was cut at the length the backend keeps.
type AgentStatus struct {
	AgentSession
	Reply          string
	ReplyTruncated bool
}

// How Reach talks to an agent it runs as a process in a local workspace.
const (
	ProtocolClaude = "claude" // Claude Code stream-json
	ProtocolCodex  = "codex"  // Codex app-server
	ProtocolACP    = "acp"    // Agent Client Protocol, such as DSH
)

// Agent is an agent a node may start in its workspaces, as configured. Role
// says what it is for; callers choose agents by it. Command[0] is an
// absolute path. On Orca, Command is typed into a terminal's shell, each
// argument quoted. In a local workspace, Command runs as a process of the
// daemon speaking Protocol, with an environment of Env and a fixed PATH,
// LANG and HOME.
type Agent struct {
	ID       string            `json:"id"`
	Role     string            `json:"role"`
	Command  []string          `json:"command"`
	Protocol string            `json:"protocol,omitempty"`
	Env      map[string]string `json:"env,omitempty"`
}

// AgentInfo is what a caller sees of an agent it may start.
type AgentInfo struct {
	ID   string `json:"id"`
	Role string `json:"role"`
}
