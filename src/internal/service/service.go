// Package service implements Reach operations: it authorizes every request,
// makes side effects idempotent by operation ID, records what happened and
// writes audit records. Decisions are the pure rules of package core; every
// side effect goes through the ports in ports.go. Transports (the local API
// now, remote MCP later) only decode requests and call this package.
package service

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/takezoh/syntoria-reach/src/internal/config"
	"github.com/takezoh/syntoria-reach/src/internal/core"
)

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

// Client is an authenticated caller. Its permissions apply to every
// workspace.
type Client struct {
	ID    string
	token string
	perms []string
}

// ws is a workspace Orca reported, which the caller may use. The request
// holds it until release, which keeps cleanup from removing it meanwhile.
type ws struct {
	id       string
	root     string
	commands []string
	agents   []string
	files    Files
	release  func()
}

// Service is the Reach application service.
type Service struct {
	store      Store
	openFiles  OpenFiles
	runCommand RunCommand
	jobs       Jobs
	orca       Orca
	folders    Folders
	newID      NewID

	workspaces string   // folder of the workspace folders Reach creates
	commands   []string // executables run_command and start_job may run
	agentIDs   []string // agents start_agent_session may start
	harness    map[string]config.Agent
	clients    []*Client

	filesMu sync.Mutex
	files   map[string]openRoot // open roots of workspaces, by workspace ID

	useMu    sync.Mutex
	users    map[string]int  // requests holding a workspace, by workspace ID
	removing map[string]bool // workspaces cleanup is removing
}

// openRoot is the open root of a workspace at the path Orca last reported.
type openRoot struct {
	path  string
	files Files
}

// New returns the service for cfg using the adapters in d.
func New(cfg *config.Config, d Deps) *Service {
	s := &Service{
		store:      d.Store,
		openFiles:  d.OpenFiles,
		runCommand: d.RunCommand,
		jobs:       d.Jobs,
		orca:       d.Orca,
		folders:    d.Folders,
		newID:      d.NewID,
		workspaces: cfg.Workspaces,
		commands:   cfg.Commands,
		harness:    map[string]config.Agent{},
		files:      map[string]openRoot{},
		users:      map[string]int{},
		removing:   map[string]bool{},
	}
	for _, a := range cfg.Agents {
		s.harness[a.ID] = a
		s.agentIDs = append(s.agentIDs, a.ID)
	}
	for _, c := range cfg.Clients {
		s.clients = append(s.clients, &Client{ID: c.ID, token: c.Token, perms: c.Allow})
	}
	return s
}

// Close releases open workspace roots.
func (s *Service) Close() {
	s.filesMu.Lock()
	defer s.filesMu.Unlock()
	for id, r := range s.files {
		r.files.Close()
		delete(s.files, id)
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
		return nil, core.Errorf(core.CodeUnauthorized, "invalid token")
	}
	return found, nil
}

// ClientByID returns the configured client with that ID. The remote
// connector acts as one configured client: the gateway has authenticated
// the remote caller, and the client's permissions bound what it may do.
func (s *Service) ClientByID(id string) (*Client, bool) {
	for _, c := range s.clients {
		if c.ID == id {
			return c, true
		}
	}
	return nil, false
}

// authorize resolves a workspace on which the client holds perm, reading it
// from Orca.
func (s *Service) authorize(ctx context.Context, c *Client, workspaceID, perm string) (*ws, error) {
	if !slices.Contains(c.perms, perm) {
		return nil, core.Errorf(core.CodePermissionDenied, "%s is not allowed", perm)
	}
	release, ok := s.use(workspaceID)
	if !ok {
		return nil, core.Errorf(core.CodeInvalidState, "workspace is being removed")
	}
	r, err := s.repo(ctx, workspaceID)
	if err != nil {
		release()
		return nil, err
	}
	f, err := s.workspaceFiles(workspaceID, r.Path)
	if err != nil {
		release()
		return nil, internal(err)
	}
	return &ws{id: workspaceID, root: r.Path, commands: s.commands, agents: s.agentIDs, files: f, release: release}, nil
}

// workspaceFiles returns the open root of a workspace at path, reopening it
// when Orca reports another path than the one opened.
func (s *Service) workspaceFiles(id, path string) (Files, error) {
	s.filesMu.Lock()
	defer s.filesMu.Unlock()
	if r, ok := s.files[id]; ok {
		if r.path == path {
			return r.files, nil
		}
		r.files.Close()
		delete(s.files, id)
	}
	f, err := s.openFiles(path)
	if err != nil {
		return nil, fmt.Errorf("open workspace %s: %w", id, err)
	}
	s.files[id] = openRoot{path: path, files: f}
	return f, nil
}

func (s *Service) closeFiles(id string) {
	s.filesMu.Lock()
	defer s.filesMu.Unlock()
	if r, ok := s.files[id]; ok {
		r.files.Close()
		delete(s.files, id)
	}
}

// ListFilesRequest lists one directory.
type ListFilesRequest struct {
	Workspace string `json:"workspace"`
	Path      string `json:"path"`
}

// ListFilesResult is a directory listing.
type ListFilesResult struct {
	Entries   []core.FileEntry `json:"entries"`
	Truncated bool             `json:"truncated"`
}

// ListFiles lists a directory in a workspace.
func (s *Service) ListFiles(ctx context.Context, c *Client, req ListFilesRequest) (res *ListFilesResult, err error) {
	rec := core.AuditRecord{ClientID: c.ID, Action: "list_files", WorkspaceID: req.Workspace, Target: req.Path}
	defer func() { s.audit(rec, err) }()
	w, err := s.authorize(ctx, c, req.Workspace, core.PermFileRead)
	if err != nil {
		return nil, err
	}
	defer w.release()
	entries, truncated, err := w.files.List(req.Path, maxListEntries)
	if err != nil {
		return nil, classify(err)
	}
	if entries == nil {
		entries = []core.FileEntry{}
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
func (s *Service) ReadFile(ctx context.Context, c *Client, req ReadFileRequest) (res *core.FileChunk, err error) {
	rec := core.AuditRecord{ClientID: c.ID, Action: "read_file", WorkspaceID: req.Workspace, Target: req.Path}
	defer func() { s.audit(rec, err) }()
	w, err := s.authorize(ctx, c, req.Workspace, core.PermFileRead)
	if err != nil {
		return nil, err
	}
	defer w.release()
	if req.Limit == 0 {
		req.Limit = defaultReadSize
	}
	if req.Offset < 0 || req.Limit < 0 || req.Limit > maxReadSize {
		return nil, core.Errorf(core.CodeInvalidRequest, "offset must be >= 0 and limit between 1 and %d", maxReadSize)
	}
	res, err = w.files.Read(req.Path, req.Offset, req.Limit)
	if err != nil {
		return nil, classify(err)
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
func (s *Service) WriteFile(ctx context.Context, c *Client, req WriteFileRequest) (*core.Operation, error) {
	rec := core.AuditRecord{ClientID: c.ID, Action: "write_file", WorkspaceID: req.Workspace, Target: req.Path, OperationID: req.OperationID}
	w, err := s.authorize(ctx, c, req.Workspace, core.PermFileWrite)
	if err == nil {
		defer w.release()
	}
	if err == nil && len(req.Content) > maxWriteSize {
		err = core.Errorf(core.CodeInvalidRequest, "content exceeds %d bytes", maxWriteSize)
	}
	if err != nil {
		s.audit(rec, err)
		return nil, err
	}
	return s.operation(c, "write_file", req.OperationID, req, rec, func() (any, error) {
		version, err := w.files.Write(req.Path, req.Content, req.ExpectedVersion)
		if err != nil {
			return nil, classify(err)
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
func (s *Service) RunCommand(ctx context.Context, c *Client, req RunCommandRequest) (*core.Operation, error) {
	rec := core.AuditRecord{ClientID: c.ID, Action: "run_command", WorkspaceID: req.Workspace, Target: req.Executable, OperationID: req.OperationID}
	w, timeout, err := s.authorizeExecutable(ctx, c, req.Workspace, req.Executable, req.TimeoutSeconds, defaultTimeout, maxTimeout)
	if err != nil {
		s.audit(rec, err)
		return nil, err
	}
	defer w.release()
	ctx = context.WithoutCancel(ctx)
	return s.operation(c, "run_command", req.OperationID, req, rec, func() (any, error) {
		res, err := s.runCommand(ctx, core.CommandSpec{
			Executable:  req.Executable,
			Args:        req.Args,
			Dir:         w.root,
			Timeout:     timeout,
			OutputLimit: outputLimit,
		})
		if err != nil {
			return nil, core.Errorf(core.CodeInvalidRequest, "start %s: %v", req.Executable, err)
		}
		return res, nil
	})
}

// authorizeExecutable resolves a workspace on which the client may run
// executable and the requested timeout. The caller releases the workspace.
func (s *Service) authorizeExecutable(ctx context.Context, c *Client, workspaceID, executable string, seconds int, def, max time.Duration) (*ws, time.Duration, error) {
	w, err := s.authorize(ctx, c, workspaceID, core.PermCommandRun)
	if err != nil {
		return nil, 0, err
	}
	if !slices.Contains(w.commands, executable) {
		w.release()
		return nil, 0, core.Errorf(core.CodePermissionDenied, "executable %q is not allowed on workspace %q", executable, workspaceID)
	}
	timeout, err := core.Timeout(seconds, def, max)
	if err != nil {
		w.release()
		return nil, 0, err
	}
	return w, timeout, nil
}

// GetOperationRequest looks up an operation by ID.
type GetOperationRequest struct {
	OperationID string `json:"operation_id"`
}

// GetOperation returns one of the client's operations.
func (s *Service) GetOperation(c *Client, req GetOperationRequest) (op *core.Operation, err error) {
	rec := core.AuditRecord{ClientID: c.ID, Action: "get_operation", OperationID: req.OperationID}
	defer func() { s.audit(rec, err) }()
	op, err = s.store.GetOperation(c.ID, req.OperationID)
	if errors.Is(err, core.ErrNotFound) {
		return nil, core.Errorf(core.CodeNotFound, "operation %q not found", req.OperationID)
	}
	if err != nil {
		return nil, internal(err)
	}
	return op, nil
}

// operation admits opID, runs do at most once, and records its outcome.
// A retry with the same ID and arguments returns the stored operation; a
// retry with different arguments is an operation_conflict.
func (s *Service) operation(c *Client, kind, opID string, req any, rec core.AuditRecord, do func() (any, error)) (op *core.Operation, err error) {
	replayed, failed, failCode := false, false, ""
	defer func() { s.auditOperation(rec, err, replayed, failed, failCode) }()
	if !core.ValidOperationID(opID) {
		return nil, core.Errorf(core.CodeInvalidRequest, "operation_id must be 1 to 128 letters, digits or ._:-")
	}
	hash, err := core.ArgsHash(req)
	if err != nil {
		return nil, internal(err)
	}
	op, created, err := s.store.Admit(c.ID, opID, kind, hash)
	if err != nil {
		return nil, internal(err)
	}
	switch core.Admit(op, created, kind, hash) {
	case core.AdmitConflict:
		return nil, core.Errorf(core.CodeOperationConflict, "operation %q was used with different arguments", opID)
	case core.AdmitReplay:
		replayed = true
		return op, nil
	}

	result, doErr := do()
	state, code, msg := core.StateSucceeded, "", ""
	if doErr != nil {
		e := classify(doErr)
		state, code, msg = core.StateFailed, e.Code, e.Message
		failed, failCode = true, code
	}
	op, err = s.store.Finish(c.ID, opID, state, result, code, msg)
	if err != nil {
		// The side effect happened but its outcome was not stored; the
		// operation stays running and becomes unknown on restart.
		return nil, internal(err)
	}
	return op, nil
}

// classify maps an adapter error to an error class; unexpected errors are
// logged and reported as internal.
func classify(err error) *core.Error {
	if e := core.Classify(err); e != nil {
		return e
	}
	return internal(err)
}

func internal(err error) *core.Error {
	slog.Error("internal error", "err", err)
	return &core.Error{Code: core.CodeInternal, Message: "internal error"}
}

// audit records a request that returned err.
func (s *Service) audit(rec core.AuditRecord, err error) {
	s.auditOperation(rec, err, false, false, "")
}

// auditOperation records an operation request: replayed if it returned a
// stored operation, failed (with failCode) if it ran and failed.
func (s *Service) auditOperation(rec core.AuditRecord, err error, replayed, failed bool, failCode string) {
	rec.Outcome, rec.ErrorCode = core.AuditOutcome(err, replayed, failed, failCode)
	if aerr := s.store.Audit(rec); aerr != nil {
		slog.Error("audit write failed", "err", aerr, "action", rec.Action)
	}
}
