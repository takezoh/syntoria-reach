package service

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"
	"time"

	"github.com/takezoh/syntoria-reach/src/internal/core"
)

// A workspace is one unit of work, kept by Orca as a folder project whose
// name has core.WorkspacePrefix. Reach creates its folder under the
// workspaces folder and registers it; its note and status are Orca's comment
// and workspace status of the project's root entry. Reach keeps nothing about
// workspaces and reads them from Orca on every request. Completed workspaces
// are removed by Cleanup once idle for core.CleanupAfter.

// Capabilities is what the client may do in any workspace.
type Capabilities struct {
	Permissions []string `json:"permissions"`
	Commands    []string `json:"commands,omitempty"`
	Agents      []string `json:"agents,omitempty"`
}

// ListCapabilities returns the client's permissions, with the commands and
// agents it may run when it holds the permission to run them.
func (s *Service) ListCapabilities(c *Client) Capabilities {
	out := Capabilities{Permissions: slices.Clone(c.perms)}
	if out.Permissions == nil {
		out.Permissions = []string{}
	}
	if slices.Contains(c.perms, core.PermCommandRun) {
		out.Commands = s.commands
	}
	if slices.Contains(c.perms, core.PermAgentRun) {
		out.Agents = s.agentIDs
	}
	s.audit(core.AuditRecord{ClientID: c.ID, Action: "list_capabilities"}, nil)
	return out
}

// ListWorkspaces returns every workspace, by name, to a client holding any
// permission.
func (s *Service) ListWorkspaces(ctx context.Context, c *Client) (out []*core.Workspace, err error) {
	defer func() { s.audit(core.AuditRecord{ClientID: c.ID, Action: "list_workspaces"}, err) }()
	out = []*core.Workspace{}
	if len(c.perms) == 0 {
		return out, nil
	}
	repos, roots, err := s.workspaceProjects(ctx)
	if err != nil {
		return nil, err
	}
	for i := range repos {
		out = append(out, core.NewWorkspace(&repos[i], roots[repos[i].ID]))
	}
	slices.SortFunc(out, func(a, b *core.Workspace) int { return cmp.Or(cmp.Compare(a.Name, b.Name), cmp.Compare(a.ID, b.ID)) })
	return out, nil
}

// CreateWorkspaceRequest creates a workspace for one new unit of work. Name
// becomes the Orca project and folder name, prefixed with
// core.WorkspacePrefix; Note says what the work is.
type CreateWorkspaceRequest struct {
	OperationID string `json:"operation_id"`
	Name        string `json:"name"`
	Note        string `json:"note"`
}

// CreateWorkspace creates the workspace's folder and registers it in Orca
// with the note, once per operation ID. The result is the workspace.
func (s *Service) CreateWorkspace(ctx context.Context, c *Client, req CreateWorkspaceRequest) (*core.Operation, error) {
	rec := core.AuditRecord{ClientID: c.ID, Action: "create_workspace", Target: req.Name, OperationID: req.OperationID}
	name, nameOK := core.WorkspaceName(req.Name)
	var err error
	switch {
	case !slices.Contains(c.perms, core.PermWorkspaceCreate):
		err = core.Errorf(core.CodePermissionDenied, "%s is not allowed", core.PermWorkspaceCreate)
	case !nameOK:
		err = core.Errorf(core.CodeInvalidRequest, "name must be letters, digits or ._-, at most 64 with the %q prefix Reach adds", core.WorkspacePrefix)
	default:
		err = core.CheckNote(req.Note)
	}
	if err != nil {
		s.audit(rec, err)
		return nil, err
	}
	ctx = context.WithoutCancel(ctx)
	return s.operation(c, "create_workspace", req.OperationID, req, rec, func() (any, error) {
		path := filepath.Join(s.workspaces, name)
		if err := s.folders.Make(path); err != nil {
			return nil, core.Errorf(core.CodeInvalidState, "cannot create the workspace folder: %v", err)
		}
		id, err := s.orca.AddFolder(ctx, path)
		if err != nil {
			// The folder is new and empty; nothing else refers to it.
			if rerr := s.folders.Remove(path); rerr != nil {
				slog.Warn("cannot remove the folder of a workspace Orca refused", "path", path, "err", rerr)
			}
			return nil, orcaError(err)
		}
		r, root, err := s.workspace(ctx, id)
		if err != nil {
			return nil, err
		}
		if err := s.orca.SetWorktree(ctx, root.ID, req.Note, ""); err != nil {
			return nil, orcaError(err)
		}
		root.Comment = req.Note
		return core.NewWorkspace(r, root), nil
	})
}

// UpdateWorkspaceRequest changes a workspace's note, its Orca workspace
// status (todo, in-progress, in-review, completed or a custom one), or both.
type UpdateWorkspaceRequest struct {
	OperationID string `json:"operation_id"`
	Workspace   string `json:"workspace"`
	Note        string `json:"note"`
	Status      string `json:"status"`
}

// UpdateWorkspace updates a workspace in Orca once per operation ID. The
// result is the workspace.
func (s *Service) UpdateWorkspace(ctx context.Context, c *Client, req UpdateWorkspaceRequest) (*core.Operation, error) {
	rec := core.AuditRecord{ClientID: c.ID, Action: "update_workspace", WorkspaceID: req.Workspace, OperationID: req.OperationID}
	var err error
	switch {
	case !slices.Contains(c.perms, core.PermWorkspaceCreate):
		err = core.Errorf(core.CodePermissionDenied, "%s is not allowed", core.PermWorkspaceCreate)
	case req.Note == "" && req.Status == "":
		err = core.Errorf(core.CodeInvalidRequest, "set note, status or both")
	case req.Status != "" && !core.ValidWorkspaceStatus(req.Status):
		err = core.Errorf(core.CodeInvalidRequest, "status must be an Orca workspace status such as todo, in-progress, in-review or completed")
	case req.Note != "":
		err = core.CheckNote(req.Note)
	}
	if err != nil {
		s.audit(rec, err)
		return nil, err
	}
	ctx = context.WithoutCancel(ctx)
	return s.operation(c, "update_workspace", req.OperationID, req, rec, func() (any, error) {
		r, root, err := s.workspace(ctx, req.Workspace)
		if err != nil {
			return nil, err
		}
		if err := s.orca.SetWorktree(ctx, root.ID, req.Note, req.Status); err != nil {
			return nil, orcaError(err)
		}
		if req.Note != "" {
			root.Comment = req.Note
		}
		if req.Status != "" {
			root.WorkspaceStatus = req.Status
		}
		return core.NewWorkspace(r, root), nil
	})
}

// reopen sets a completed workspace back in progress: work resumed in it.
func (s *Service) reopen(ctx context.Context, id string) {
	_, root, err := s.workspace(ctx, id)
	if err == nil && root.WorkspaceStatus == core.WorkspaceCompleted {
		err = s.orca.SetWorktree(ctx, root.ID, "", core.WorkspaceInProgress)
	}
	if err != nil {
		slog.Warn("cannot set the workspace in progress", "workspace", id, "err", err)
	}
}

// Cleanup removes the workspaces that are completed and have had no
// activity, in Orca or through Reach, for core.CleanupAfter: their Orca
// projects and the folders Reach created for them. It returns the IDs of
// the workspaces removed.
func (s *Service) Cleanup(ctx context.Context, now time.Time) ([]string, error) {
	repos, roots, err := s.workspaceProjects(ctx)
	if err != nil {
		return nil, err
	}
	var removed []string
	var errs []error
	for i := range repos {
		r, root := &repos[i], roots[repos[i].ID]
		if root == nil || root.WorkspaceStatus != core.WorkspaceCompleted {
			continue
		}
		if done, err := s.cleanup(ctx, r, root, now); err != nil {
			errs = append(errs, fmt.Errorf("workspace %s: %w", r.ID, err))
		} else if done {
			removed = append(removed, r.ID)
		}
	}
	return removed, errors.Join(errs...)
}

// cleanup removes one completed workspace if it is due, and reports whether
// its Orca project was removed.
func (s *Service) cleanup(ctx context.Context, r *core.Repo, root *core.Worktree, now time.Time) (bool, error) {
	if !s.markDeleting(r.ID) {
		return false, nil
	}
	defer s.unmarkDeleting(r.ID)
	busy, err := s.store.WorkspaceBusy(r.ID)
	if err != nil {
		return false, err
	}
	used, err := s.store.WorkspaceLastUsed(r.ID)
	if err != nil {
		return false, err
	}
	last := time.UnixMilli(root.LastActivityAt)
	if used.After(last) {
		last = used
	}
	if !core.Expired(root.WorkspaceStatus, last, now, busy) {
		return false, nil
	}
	s.closeFiles(r.ID)
	if err := s.orca.DeleteProject(ctx, r.ID); err != nil {
		return false, orcaError(err)
	}
	s.audit(core.AuditRecord{ClientID: "reach", Action: "cleanup_workspace", WorkspaceID: r.ID, Target: r.DisplayName}, nil)
	if core.OwnFolder(s.workspaces, r.Path, r.DisplayName) {
		if err := s.folders.Remove(r.Path); err != nil {
			return true, fmt.Errorf("remove folder %s: %w", r.Path, err)
		}
	}
	return true, nil
}

// workspaceProjects reads the projects that are workspaces and their root
// entries, by repo ID, from Orca.
func (s *Service) workspaceProjects(ctx context.Context) ([]core.Repo, map[string]*core.Worktree, error) {
	repos, err := s.orca.Repos(ctx)
	if err != nil {
		return nil, nil, orcaError(err)
	}
	repos = slices.DeleteFunc(repos, func(r core.Repo) bool { return !core.IsWorkspace(&r) })
	if len(repos) == 0 {
		return repos, nil, nil
	}
	wts, err := s.orca.Worktrees(ctx, "")
	if err != nil {
		return nil, nil, orcaError(err)
	}
	roots := map[string]*core.Worktree{}
	for i := range wts {
		if wts[i].IsMainWorktree {
			roots[wts[i].RepoID] = &wts[i]
		}
	}
	return repos, roots, nil
}

// repo reads the project of workspace id from Orca.
func (s *Service) repo(ctx context.Context, id string) (*core.Repo, error) {
	r, err := s.orca.Repo(ctx, id)
	if errors.Is(err, core.ErrNotFound) || (err == nil && !core.IsWorkspace(r)) {
		return nil, core.Errorf(core.CodeNotFound, "workspace %q not found", id)
	}
	if err != nil {
		return nil, orcaError(err)
	}
	return r, nil
}

// workspace reads the project of workspace id and its root entry from Orca.
func (s *Service) workspace(ctx context.Context, id string) (*core.Repo, *core.Worktree, error) {
	r, err := s.repo(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	wts, err := s.orca.Worktrees(ctx, id)
	if errors.Is(err, core.ErrNotFound) {
		return nil, nil, core.Errorf(core.CodeNotFound, "workspace %q not found", id)
	}
	if err != nil {
		return nil, nil, orcaError(err)
	}
	for i := range wts {
		if wts[i].IsMainWorktree {
			return r, &wts[i], nil
		}
	}
	return nil, nil, core.Errorf(core.CodeOrcaFailed, "Orca reported no root entry for workspace %q", id)
}

func (s *Service) markDeleting(id string) bool {
	s.deletingMu.Lock()
	defer s.deletingMu.Unlock()
	if s.deleting[id] {
		return false
	}
	s.deleting[id] = true
	return true
}

func (s *Service) unmarkDeleting(id string) {
	s.deletingMu.Lock()
	defer s.deletingMu.Unlock()
	delete(s.deleting, id)
}

func (s *Service) isDeleting(id string) bool {
	s.deletingMu.Lock()
	defer s.deletingMu.Unlock()
	return s.deleting[id]
}

// orcaError classifies a failed Orca call; every failure is orca_failed.
func orcaError(err error) error {
	if e := core.Classify(err); e != nil && e.Code == core.CodeOrcaFailed {
		return e
	}
	return core.Errorf(core.CodeOrcaFailed, "%v", err)
}
