package core

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

var (
	operationIDPattern   = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)
	workspaceNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
)

// ValidOperationID reports whether id can name an operation.
func ValidOperationID(id string) bool { return operationIDPattern.MatchString(id) }

// ValidWorkspaceName reports whether name can name a workspace (it becomes
// the worktree and branch name).
func ValidWorkspaceName(name string) bool { return workspaceNamePattern.MatchString(name) }

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

// WorkspaceAccess reports whether a client holding perms on the workspace's
// project may perform perm on it. Only ready workspaces of a configured
// project are usable.
func WorkspaceAccess(w *Workspace, projectKnown bool, perms []string, perm string) bool {
	return projectKnown && w.State == WorkspaceReady && slices.Contains(perms, perm)
}

// Deletable decides whether a workspace may be deleted now.
func Deletable(w *Workspace, busy bool) error {
	switch {
	case w.State != WorkspaceReady:
		return Errorf(CodeInvalidState, "workspace is %s", w.State)
	case busy:
		return Errorf(CodeInvalidState, "workspace has running jobs or active agent sessions")
	}
	return nil
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
