// Package service implements Reach operations: it authorizes every request,
// makes side effects idempotent by operation ID, and writes audit records.
// Transports (the local API now, remote MCP later) only decode requests and
// call this package.
package service

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"regexp"
	"slices"
	"sync"
	"time"

	"github.com/takezoh/syntoria-reach/src/internal/agent"
	"github.com/takezoh/syntoria-reach/src/internal/command"
	"github.com/takezoh/syntoria-reach/src/internal/config"
	"github.com/takezoh/syntoria-reach/src/internal/job"
	"github.com/takezoh/syntoria-reach/src/internal/orca"
	"github.com/takezoh/syntoria-reach/src/internal/store"
	"github.com/takezoh/syntoria-reach/src/internal/workspace"
)

// Error codes returned to callers.
const (
	CodeInvalidRequest    = "invalid_request"
	CodeUnauthorized      = "unauthorized"
	CodePermissionDenied  = "permission_denied"
	CodeNotFound          = "not_found"
	CodeOperationConflict = "operation_conflict"
	CodeVersionConflict   = "version_conflict"
	CodeInvalidState      = "invalid_state" // e.g. a turn is already running
	CodeHarnessFailed     = "harness_unavailable"
	CodeOrcaFailed        = "orca_failed"
	CodeInternal          = "internal"
)

// Error is a classified failure.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

func errorf(code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// Limits.
const (
	maxListEntries  = 1000
	defaultReadSize = 1 << 20
	maxReadSize     = 4 << 20
	maxWriteSize    = 8 << 20
	defaultTimeout  = 60 * time.Second
	maxTimeout      = 600 * time.Second
	outputLimit     = 1 << 20
)

var operationIDPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)

// Client is an authenticated caller.
type Client struct {
	ID    string
	token string
	perms map[string][]string // project ID -> permissions
}

// ws is a resolved, ready workspace the caller may use.
type ws struct {
	id       string
	root     string
	commands []string
	agents   []string
	fs       *workspace.FS
}

// Service is the Reach application service.
type Service struct {
	store    *store.Store
	orca     *orca.CLI
	projects map[string]config.Project
	agents   map[string]config.Agent
	clients  []*Client
	jobs     *job.Manager
	sessions *agent.Manager

	fsMu sync.Mutex
	fs   map[string]*workspace.FS // open roots of ready workspaces
}

// New returns the service. Job output and harness logs are kept next to the
// database.
func New(cfg *config.Config, st *store.Store) (*Service, error) {
	stateDir := filepath.Dir(cfg.Database)
	s := &Service{
		store:    st,
		orca:     &orca.CLI{Command: cfg.Orca},
		projects: map[string]config.Project{},
		agents:   map[string]config.Agent{},
		jobs:     job.New(st, filepath.Join(stateDir, "jobs")),
		sessions: agent.New(st, filepath.Join(stateDir, "agents")),
		fs:       map[string]*workspace.FS{},
	}
	for _, a := range cfg.Agents {
		s.agents[a.ID] = a
	}
	for _, p := range cfg.Projects {
		s.projects[p.ID] = p
	}
	for _, c := range cfg.Clients {
		cl := &Client{ID: c.ID, token: c.Token, perms: map[string][]string{}}
		for _, g := range c.Grants {
			cl.perms[g.Project] = append(cl.perms[g.Project], g.Allow...)
		}
		s.clients = append(s.clients, cl)
	}
	return s, nil
}

// Close releases open workspace roots.
func (s *Service) Close() {
	s.fsMu.Lock()
	defer s.fsMu.Unlock()
	for id, f := range s.fs {
		f.Close()
		delete(s.fs, id)
	}
}

// Authenticate resolves a bearer token to a client.
func (s *Service) Authenticate(token string) (*Client, error) {
	var found *Client
	for _, c := range s.clients {
		if subtle.ConstantTimeCompare([]byte(c.token), []byte(token)) == 1 {
			found = c
		}
	}
	if found == nil {
		return nil, errorf(CodeUnauthorized, "invalid token")
	}
	return found, nil
}

// authorize resolves one of the client's ready workspaces on which the
// client holds perm (through the workspace's project).
func (s *Service) authorize(c *Client, workspaceID, perm string) (*ws, error) {
	// Unknown, foreign and forbidden workspaces are indistinguishable.
	denied := errorf(CodePermissionDenied, "%s is not allowed on workspace %q", perm, workspaceID)
	rec, err := s.store.GetWorkspace(c.ID, workspaceID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, denied
	}
	if err != nil {
		return nil, internal(err)
	}
	p, ok := s.projects[rec.ProjectID]
	if !ok || rec.State != store.WorkspaceReady || !slices.Contains(c.perms[rec.ProjectID], perm) {
		return nil, denied
	}
	f, err := s.openFS(rec)
	if err != nil {
		return nil, internal(err)
	}
	return &ws{id: rec.ID, root: rec.Path, commands: p.Commands, agents: p.Agents, fs: f}, nil
}

func (s *Service) openFS(rec *store.Workspace) (*workspace.FS, error) {
	s.fsMu.Lock()
	defer s.fsMu.Unlock()
	if f, ok := s.fs[rec.ID]; ok {
		return f, nil
	}
	f, err := workspace.Open(rec.Path)
	if err != nil {
		return nil, fmt.Errorf("open workspace %s: %w", rec.ID, err)
	}
	s.fs[rec.ID] = f
	return f, nil
}

func (s *Service) closeFS(id string) {
	s.fsMu.Lock()
	defer s.fsMu.Unlock()
	if f, ok := s.fs[id]; ok {
		f.Close()
		delete(s.fs, id)
	}
}

// ListFilesRequest lists one directory.
type ListFilesRequest struct {
	Workspace string `json:"workspace"`
	Path      string `json:"path"`
}

// ListFilesResult is a directory listing.
type ListFilesResult struct {
	Entries   []workspace.Entry `json:"entries"`
	Truncated bool              `json:"truncated"`
}

// ListFiles lists a directory in a workspace.
func (s *Service) ListFiles(c *Client, req ListFilesRequest) (res *ListFilesResult, err error) {
	rec := store.AuditRecord{ClientID: c.ID, Action: "list_files", WorkspaceID: req.Workspace, Target: req.Path}
	defer func() { s.audit(rec, err) }()
	w, err := s.authorize(c, req.Workspace, config.PermFileRead)
	if err != nil {
		return nil, err
	}
	entries, truncated, err := w.fs.List(req.Path, maxListEntries)
	if err != nil {
		return nil, fileError(err)
	}
	if entries == nil {
		entries = []workspace.Entry{}
	}
	return &ListFilesResult{Entries: entries, Truncated: truncated}, nil
}

// ReadFileRequest reads part of a file. Limit 0 means the default size.
type ReadFileRequest struct {
	Workspace string `json:"workspace"`
	Path      string `json:"path"`
	Offset    int64  `json:"offset"`
	Limit     int    `json:"limit"`
}

// ReadFile reads a bounded part of a file.
func (s *Service) ReadFile(c *Client, req ReadFileRequest) (res *workspace.ReadResult, err error) {
	rec := store.AuditRecord{ClientID: c.ID, Action: "read_file", WorkspaceID: req.Workspace, Target: req.Path}
	defer func() { s.audit(rec, err) }()
	w, err := s.authorize(c, req.Workspace, config.PermFileRead)
	if err != nil {
		return nil, err
	}
	if req.Limit == 0 {
		req.Limit = defaultReadSize
	}
	if req.Offset < 0 || req.Limit < 0 || req.Limit > maxReadSize {
		return nil, errorf(CodeInvalidRequest, "offset must be >= 0 and limit between 1 and %d", maxReadSize)
	}
	res, err = w.fs.Read(req.Path, req.Offset, req.Limit)
	if err != nil {
		return nil, fileError(err)
	}
	return res, nil
}

// WriteFileRequest replaces a file. ExpectedVersion "" means the file must
// not exist yet; otherwise it must match the version returned by ReadFile.
type WriteFileRequest struct {
	OperationID     string `json:"operation_id"`
	Workspace       string `json:"workspace"`
	Path            string `json:"path"`
	Content         []byte `json:"content"`
	ExpectedVersion string `json:"expected_version"`
}

// WriteFileResult is the result of a successful write.
type WriteFileResult struct {
	Version string `json:"version"`
}

// WriteFile writes a file once per operation ID.
func (s *Service) WriteFile(c *Client, req WriteFileRequest) (*store.Operation, error) {
	rec := store.AuditRecord{ClientID: c.ID, Action: "write_file", WorkspaceID: req.Workspace, Target: req.Path, OperationID: req.OperationID}
	w, err := s.authorize(c, req.Workspace, config.PermFileWrite)
	if err == nil && len(req.Content) > maxWriteSize {
		err = errorf(CodeInvalidRequest, "content exceeds %d bytes", maxWriteSize)
	}
	if err != nil {
		s.audit(rec, err)
		return nil, err
	}
	return s.operation(c, "write_file", req.OperationID, req, rec, func() (any, error) {
		version, err := w.fs.Write(req.Path, req.Content, req.ExpectedVersion)
		if err != nil {
			return nil, fileError(err)
		}
		return WriteFileResult{Version: version}, nil
	})
}

// RunCommandRequest runs an allowed executable in the workspace root.
// TimeoutSeconds 0 means the default timeout.
type RunCommandRequest struct {
	OperationID    string   `json:"operation_id"`
	Workspace      string   `json:"workspace"`
	Executable     string   `json:"executable"`
	Args           []string `json:"args"`
	TimeoutSeconds int      `json:"timeout_seconds"`
}

// RunCommand runs a command once per operation ID and waits for it.
// The command keeps running if the caller disconnects.
func (s *Service) RunCommand(ctx context.Context, c *Client, req RunCommandRequest) (*store.Operation, error) {
	rec := store.AuditRecord{ClientID: c.ID, Action: "run_command", WorkspaceID: req.Workspace, Target: req.Executable, OperationID: req.OperationID}
	w, err := s.authorize(c, req.Workspace, config.PermCommandRun)
	if err == nil && !slices.Contains(w.commands, req.Executable) {
		err = errorf(CodePermissionDenied, "executable %q is not allowed on workspace %q", req.Executable, req.Workspace)
	}
	timeout := time.Duration(req.TimeoutSeconds) * time.Second
	if req.TimeoutSeconds == 0 {
		timeout = defaultTimeout
	}
	if err == nil && (timeout <= 0 || timeout > maxTimeout) {
		err = errorf(CodeInvalidRequest, "timeout_seconds must be between 1 and %d", int(maxTimeout.Seconds()))
	}
	if err != nil {
		s.audit(rec, err)
		return nil, err
	}
	ctx = context.WithoutCancel(ctx)
	return s.operation(c, "run_command", req.OperationID, req, rec, func() (any, error) {
		res, err := command.Run(ctx, command.Spec{
			Executable:  req.Executable,
			Args:        req.Args,
			Dir:         w.root,
			Timeout:     timeout,
			OutputLimit: outputLimit,
		})
		if err != nil {
			return nil, errorf(CodeInvalidRequest, "start %s: %v", req.Executable, err)
		}
		return res, nil
	})
}

// GetOperationRequest looks up an operation by ID.
type GetOperationRequest struct {
	OperationID string `json:"operation_id"`
}

// GetOperation returns one of the client's operations.
func (s *Service) GetOperation(c *Client, req GetOperationRequest) (op *store.Operation, err error) {
	rec := store.AuditRecord{ClientID: c.ID, Action: "get_operation", OperationID: req.OperationID}
	defer func() { s.audit(rec, err) }()
	op, err = s.store.GetOperation(c.ID, req.OperationID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, errorf(CodeNotFound, "operation %q not found", req.OperationID)
	}
	if err != nil {
		return nil, internal(err)
	}
	return op, nil
}

// operation admits opID, runs do at most once, and records its outcome.
// A retry with the same ID and arguments returns the stored operation; a
// retry with different arguments is an operation_conflict.
func (s *Service) operation(c *Client, kind, opID string, req any, rec store.AuditRecord, do func() (any, error)) (op *store.Operation, err error) {
	replayed := false
	defer func() {
		if replayed && err == nil {
			rec.Outcome = "replayed"
		}
		s.audit(rec, err)
	}()
	if !operationIDPattern.MatchString(opID) {
		return nil, errorf(CodeInvalidRequest, "operation_id must match %s", operationIDPattern)
	}
	hash, err := argsHash(req)
	if err != nil {
		return nil, internal(err)
	}
	op, created, err := s.store.Admit(c.ID, opID, kind, hash)
	if err != nil {
		return nil, internal(err)
	}
	if !created {
		if op.Kind != kind || op.ArgsHash != hash {
			return nil, errorf(CodeOperationConflict, "operation %q was used with different arguments", opID)
		}
		replayed = true
		return op, nil
	}

	result, doErr := do()
	state, code, msg := store.StateSucceeded, "", ""
	if doErr != nil {
		state, code, msg = store.StateFailed, CodeInternal, doErr.Error()
		var e *Error
		if errors.As(doErr, &e) {
			code, msg = e.Code, e.Message
		}
		rec.Outcome, rec.ErrorCode = "failed", code
	}
	op, err = s.store.Finish(c.ID, opID, state, result, code, msg)
	if err != nil {
		// The side effect happened but its outcome was not stored; the
		// operation stays running and becomes unknown on restart.
		return nil, internal(err)
	}
	return op, nil
}

// argsHash identifies the request arguments, excluding the operation ID.
func argsHash(req any) (string, error) {
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

func fileError(err error) error {
	switch {
	case errors.Is(err, workspace.ErrInvalidPath), errors.Is(err, workspace.ErrNotRegular):
		return errorf(CodeInvalidRequest, "%v", err)
	case errors.Is(err, workspace.ErrOutside):
		return errorf(CodePermissionDenied, "%v", err)
	case errors.Is(err, workspace.ErrNotFound):
		return errorf(CodeNotFound, "%v", err)
	case errors.Is(err, workspace.ErrVersionConflict):
		return errorf(CodeVersionConflict, "%v", err)
	}
	return internal(err)
}

func internal(err error) *Error {
	slog.Error("internal error", "err", err)
	return &Error{Code: CodeInternal, Message: "internal error"}
}

func (s *Service) audit(rec store.AuditRecord, err error) {
	if rec.Outcome == "" {
		rec.Outcome = "ok"
	}
	if err != nil {
		rec.Outcome = "error"
		var e *Error
		if errors.As(err, &e) {
			rec.ErrorCode = e.Code
			if e.Code == CodeUnauthorized || e.Code == CodePermissionDenied {
				rec.Outcome = "denied"
			}
		}
	}
	if aerr := s.store.Audit(rec); aerr != nil {
		slog.Error("audit write failed", "err", aerr, "action", rec.Action)
	}
}
