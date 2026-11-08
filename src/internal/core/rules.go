package core

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

var (
	operationIDPattern     = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)
	workspaceNamePattern   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	workspaceStatusPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)
)

// MaxNoteLength bounds a workspace note, in characters.
const MaxNoteLength = 2000

// Workspace statuses Reach itself sets; Orca accepts others too.
const (
	WorkspaceInProgress = "in-progress"
	WorkspaceCompleted  = "completed"
)

// ValidOperationID reports whether id can name an operation.
func ValidOperationID(id string) bool { return operationIDPattern.MatchString(id) }

// WorkspaceName returns the name of the Orca project for a requested
// workspace name: WorkspacePrefix is prepended unless the name already has
// it. It is also the name of the workspace's folder.
func WorkspaceName(name string) (string, bool) {
	if !strings.HasPrefix(name, WorkspacePrefix) {
		name = WorkspacePrefix + name
	}
	return name, len(name) > len(WorkspacePrefix) && workspaceNamePattern.MatchString(name)
}

// IsWorkspace reports whether an Orca project is a Reach workspace.
func IsWorkspace(r *Repo) bool { return strings.HasPrefix(r.DisplayName, WorkspacePrefix) }

// NewWorkspace describes the workspace of an Orca project with its root entry
// (nil when Orca reported none).
func NewWorkspace(r *Repo, root *Worktree) *Workspace {
	w := &Workspace{ID: r.ID, Name: r.DisplayName, Path: r.Path}
	if root != nil {
		w.Note, w.Status = root.Comment, root.WorkspaceStatus
	}
	return w
}

// Expired reports whether a workspace is due for cleanup: completed, idle,
// and without activity for CleanupAfter.
func Expired(status string, lastActivity, now time.Time, busy bool) bool {
	return status == WorkspaceCompleted && !busy && now.Sub(lastActivity) >= CleanupAfter
}

// OwnFolder reports whether path is a folder Reach created for workspace
// name under base, the only folders Reach ever removes.
func OwnFolder(base, path, name string) bool {
	return strings.HasPrefix(name, WorkspacePrefix) && filepath.IsAbs(base) && path == filepath.Join(base, name) && filepath.Base(path) == name
}

// CheckNote reports whether note can describe a workspace: not blank, at
// most MaxNoteLength characters, without NUL bytes.
func CheckNote(note string) error {
	if strings.TrimSpace(note) == "" || utf8.RuneCountInString(note) > MaxNoteLength || strings.ContainsRune(note, 0) || !utf8.ValidString(note) {
		return Errorf(CodeInvalidRequest, "note must be 1 to %d characters of text", MaxNoteLength)
	}
	return nil
}

// ValidWorkspaceStatus reports whether status can name an Orca workspace
// status; Orca decides whether it exists.
func ValidWorkspaceStatus(status string) bool { return workspaceStatusPattern.MatchString(status) }

// CheckPath reports whether rel can name a file inside a workspace: a local
// path ("." for the root) without NUL bytes. Symlinks and mount points are
// the file adapter's to check.
func CheckPath(rel string) error {
	if rel == "." {
		return nil
	}
	if !filepath.IsLocal(rel) || strings.ContainsRune(rel, 0) {
		return fmt.Errorf("%w: %q", ErrInvalidPath, rel)
	}
	return nil
}

// FileVersion is the version of a file with content data.
func FileVersion(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// ArgsHash identifies a request's arguments, excluding its operation ID.
func ArgsHash(req any) (string, error) {
	b, err := json.Marshal(req)
	if err != nil {
		return "", err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(b, &fields); err != nil {
		return "", err
	}
	delete(fields, "operation_id")
	if b, err = json.Marshal(fields); err != nil { // map keys are sorted
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// Admission is what to do with a side-effecting request.
type Admission int

const (
	// AdmitRun: the request is new; perform it.
	AdmitRun Admission = iota
	// AdmitReplay: the same request was already admitted; return its record.
	AdmitReplay
	// AdmitConflict: the operation ID was used for a different request.
	AdmitConflict
)

// Admit decides what to do given the operation stored under the request's
// ID; created reports whether this request just stored it.
func Admit(stored *Operation, created bool, kind, argsHash string) Admission {
	switch {
	case created:
		return AdmitRun
	case stored.Kind != kind || stored.ArgsHash != argsHash:
		return AdmitConflict
	}
	return AdmitReplay
}

// Timeout converts a requested number of seconds (0 for the default) to a
// duration within (0, max].
func Timeout(seconds int, def, max time.Duration) (time.Duration, error) {
	if seconds == 0 {
		return def, nil
	}
	d := time.Duration(seconds) * time.Second
	if d <= 0 || d > max {
		return 0, Errorf(CodeInvalidRequest, "timeout_seconds must be between 1 and %d", int(max.Seconds()))
	}
	return d, nil
}

// JobOutcome decides a finished job's state and message.
func JobOutcome(e JobExit) (state, message string) {
	switch {
	case e.Cancelled:
		return JobCancelled, ""
	case e.TimedOut:
		return JobFailed, "timed out"
	case e.Err != "":
		return JobFailed, e.Err
	case e.ExitCode == nil:
		return JobFailed, "terminated by a signal"
	case *e.ExitCode != 0:
		return JobFailed, ""
	}
	return JobSucceeded, ""
}

// Resumable decides whether prev may be continued by a new session in the
// given workspace and agent, and returns its protocol session ID. Protocol
// IDs come only from the caller's own records, never from a request.
func Resumable(prev *AgentSession, workspaceID, agentID string) (string, error) {
	switch {
	case prev.WorkspaceID != workspaceID || prev.AgentID != agentID:
		return "", Errorf(CodeInvalidRequest, "session %q belongs to another workspace or agent", prev.ID)
	case prev.State == SessionStarting || prev.State == SessionIdle || prev.State == SessionRunning:
		return "", Errorf(CodeInvalidState, "session %q is still active; stop it first", prev.ID)
	case prev.ProtocolSessionID == "":
		return "", Errorf(CodeInvalidState, "session %q never opened a protocol session", prev.ID)
	}
	return prev.ProtocolSessionID, nil
}

// Classify maps an adapter error to an error class. It returns nil for
// errors that are not expected (internal failures).
func Classify(err error) *Error {
	var e *Error
	var oe *OrcaError
	switch {
	case errors.As(err, &e):
		return e
	case errors.As(err, &oe):
		return Errorf(CodeOrcaFailed, "%s: %s", oe.Code, oe.Message)
	case errors.Is(err, ErrInvalidPath), errors.Is(err, ErrNotRegular):
		return Errorf(CodeInvalidRequest, "%v", err)
	case errors.Is(err, ErrOutside):
		return Errorf(CodePermissionDenied, "%v", err)
	case errors.Is(err, ErrNotFound):
		return Errorf(CodeNotFound, "%v", err)
	case errors.Is(err, ErrVersionConflict):
		return Errorf(CodeVersionConflict, "%v", err)
	case errors.Is(err, ErrSessionInactive), errors.Is(err, ErrSessionBusy):
		return Errorf(CodeInvalidState, "%v", err)
	}
	return nil
}

// AuditOutcome decides how a request is recorded in the audit log.
func AuditOutcome(err error, replayed, failed bool, failCode string) (outcome, code string) {
	if err != nil {
		var e *Error
		if errors.As(err, &e) {
			if e.Code == CodeUnauthorized || e.Code == CodePermissionDenied {
				return "denied", e.Code
			}
			return "error", e.Code
		}
		return "error", ""
	}
	switch {
	case failed:
		return "failed", failCode
	case replayed:
		return "replayed", ""
	}
	return "ok", ""
}
