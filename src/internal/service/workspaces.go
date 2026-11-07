package service

import (
	"cmp"
	"context"
	"errors"
	"slices"

	"github.com/takezoh/syntoria-reach/src/internal/core"
)

// ProjectInfo describes a project the client may create workspaces from.
type ProjectInfo struct {
	ID          string   `json:"project"`
	Permissions []string `json:"permissions"`
	Commands    []string `json:"commands,omitempty"`
	Agents      []string `json:"agents,omitempty"`
}

// ListProjects returns the projects the client may create workspaces from.
func (s *Service) ListProjects(c *Client) []ProjectInfo {
	out := []ProjectInfo{}
	for id, perms := range c.perms {
		p, ok := s.projects[id]
		if !ok || !slices.Contains(perms, core.PermWorkspaceCreate) {
			continue
		}
		info := ProjectInfo{ID: id, Permissions: perms}
		if slices.Contains(perms, core.PermCommandRun) {
			info.Commands = p.Commands
		}
		if slices.Contains(perms, core.PermAgentRun) {
			info.Agents = p.Agents
		}
		out = append(out, info)
	}
	slices.SortFunc(out, func(a, b ProjectInfo) int { return cmp.Compare(a.ID, b.ID) })
	s.audit(core.AuditRecord{ClientID: c.ID, Action: "list_projects"}, nil)
	return out
}

// ListWorkspaces returns the client's ready workspaces.
func (s *Service) ListWorkspaces(c *Client) (out []*core.Workspace, err error) {
	defer func() { s.audit(core.AuditRecord{ClientID: c.ID, Action: "list_workspaces"}, err) }()
	out, err = s.store.ListWorkspaces(c.ID)
	if err != nil {
		return nil, internal(err)
	}
	return out, nil
}

// CreateWorkspaceRequest creates an Orca workspace from a project. Name
// becomes the worktree and branch name.
type CreateWorkspaceRequest struct {
	OperationID string `json:"operation_id"`
	Project     string `json:"project"`
	Name        string `json:"name"`
}

// CreateWorkspace creates an Orca worktree once per operation ID. The result
// is the workspace; only this client can use and delete it.
func (s *Service) CreateWorkspace(ctx context.Context, c *Client, req CreateWorkspaceRequest) (*core.Operation, error) {
	rec := core.AuditRecord{ClientID: c.ID, Action: "create_workspace", Target: req.Project + "/" + req.Name, OperationID: req.OperationID}
	p, ok := s.projects[req.Project]
	var err error
	if !ok || !slices.Contains(c.perms[req.Project], core.PermWorkspaceCreate) {
		err = core.Errorf(core.CodePermissionDenied, "%s is not allowed on project %q", core.PermWorkspaceCreate, req.Project)
	} else if !core.ValidWorkspaceName(req.Name) {
		err = core.Errorf(core.CodeInvalidRequest, "name must be 1 to 64 letters, digits or ._- and start with a letter or digit")
	}
	if err != nil {
		s.audit(rec, err)
		return nil, err
	}
	ctx = context.WithoutCancel(ctx)
	return s.operation(c, "create_workspace", req.OperationID, req, rec, func() (any, error) {
		id := s.newID("ws_")
		if err := s.store.CreateWorkspace(&core.Workspace{ID: id, ClientID: c.ID, ProjectID: p.ID, Name: req.Name}); err != nil {
			return nil, internal(err)
		}
		wt, err := s.orca.CreateWorktree(ctx, p.Repo, req.Name)
		if err != nil {
			if _, serr := s.store.SetWorkspaceState(id, core.WorkspaceCreating, core.WorkspaceFailed, "", "", "", err.Error()); serr != nil {
				return nil, internal(serr)
			}
			return nil, orcaError(err)
		}
		if _, err := s.store.SetWorkspaceState(id, core.WorkspaceCreating, core.WorkspaceReady, wt.ID, wt.Path, wt.Branch, ""); err != nil {
			return nil, internal(err)
		}
		return s.workspace(c, id)
	})
}

// WorkspaceRequest identifies a workspace.
type WorkspaceRequest struct {
	OperationID string `json:"operation_id"`
	Workspace   string `json:"workspace"`
}

// DeleteWorkspace removes one of the client's workspaces from Orca and Git
// once per operation ID, deleting its branch when Orca can prove it is
// merged. Workspaces with running jobs or active agent sessions, or with
// uncommitted changes, are not deleted.
func (s *Service) DeleteWorkspace(ctx context.Context, c *Client, req WorkspaceRequest) (*core.Operation, error) {
	rec := core.AuditRecord{ClientID: c.ID, Action: "delete_workspace", WorkspaceID: req.Workspace, OperationID: req.OperationID}
	w, err := s.workspace(c, req.Workspace)
	if err != nil {
		s.audit(rec, err)
		return nil, err
	}
	ctx = context.WithoutCancel(ctx)
	return s.operation(c, "delete_workspace", req.OperationID, req, rec, func() (any, error) {
		busy, err := s.store.WorkspaceBusy(w.ID)
		if err != nil {
			return nil, internal(err)
		}
		if err := core.Deletable(w, busy); err != nil {
			return nil, err
		}
		// The conditional move decides a race with another delete.
		if ok, err := s.store.SetWorkspaceState(w.ID, core.WorkspaceReady, core.WorkspaceDeleting, "", "", "", ""); err != nil {
			return nil, internal(err)
		} else if !ok {
			return nil, core.Errorf(core.CodeInvalidState, "workspace is not ready")
		}
		s.closeFiles(w.ID)
		if err := s.orca.RemoveWorktree(ctx, w.OrcaID); err != nil {
			if _, serr := s.store.SetWorkspaceState(w.ID, core.WorkspaceDeleting, core.WorkspaceReady, "", "", "", err.Error()); serr != nil {
				return nil, internal(serr)
			}
			return nil, orcaError(err)
		}
		if _, err := s.store.SetWorkspaceState(w.ID, core.WorkspaceDeleting, core.WorkspaceDeleted, "", "", "", ""); err != nil {
			return nil, internal(err)
		}
		return s.workspace(c, w.ID)
	})
}

func (s *Service) workspace(c *Client, id string) (*core.Workspace, error) {
	w, err := s.store.GetWorkspace(c.ID, id)
	if errors.Is(err, core.ErrNotFound) {
		return nil, core.Errorf(core.CodeNotFound, "workspace %q not found", id)
	}
	if err != nil {
		return nil, internal(err)
	}
	return w, nil
}

// orcaError classifies a failed Orca call; every failure is orca_failed.
func orcaError(err error) error {
	if e := core.Classify(err); e != nil && e.Code == core.CodeOrcaFailed {
		return e
	}
	return core.Errorf(core.CodeOrcaFailed, "%v", err)
}
