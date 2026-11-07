package service

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"regexp"
	"slices"

	"github.com/takezoh/syntoria-reach/src/internal/config"
	"github.com/takezoh/syntoria-reach/src/internal/orca"
	"github.com/takezoh/syntoria-reach/src/internal/store"
)

var workspaceNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

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
		if !ok || !slices.Contains(perms, config.PermWorkspaceCreate) {
			continue
		}
		info := ProjectInfo{ID: id, Permissions: perms}
		if slices.Contains(perms, config.PermCommandRun) {
			info.Commands = p.Commands
		}
		if slices.Contains(perms, config.PermAgentRun) {
			info.Agents = p.Agents
		}
		out = append(out, info)
	}
	slices.SortFunc(out, func(a, b ProjectInfo) int { return cmp.Compare(a.ID, b.ID) })
	s.audit(store.AuditRecord{ClientID: c.ID, Action: "list_projects"}, nil)
	return out
}

// ListWorkspaces returns the client's ready workspaces.
func (s *Service) ListWorkspaces(c *Client) (out []*store.Workspace, err error) {
	defer func() { s.audit(store.AuditRecord{ClientID: c.ID, Action: "list_workspaces"}, err) }()
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
func (s *Service) CreateWorkspace(ctx context.Context, c *Client, req CreateWorkspaceRequest) (*store.Operation, error) {
	rec := store.AuditRecord{ClientID: c.ID, Action: "create_workspace", Target: req.Project + "/" + req.Name, OperationID: req.OperationID}
	p, ok := s.projects[req.Project]
	var err error
	if !ok || !slices.Contains(c.perms[req.Project], config.PermWorkspaceCreate) {
		err = errorf(CodePermissionDenied, "%s is not allowed on project %q", config.PermWorkspaceCreate, req.Project)
	} else if !workspaceNamePattern.MatchString(req.Name) {
		err = errorf(CodeInvalidRequest, "name must match %s", workspaceNamePattern)
	}
	if err != nil {
		s.audit(rec, err)
		return nil, err
	}
	ctx = context.WithoutCancel(ctx)
	return s.operation(c, "create_workspace", req.OperationID, req, rec, func() (any, error) {
		id := "ws_" + randomHex()
		if err := s.store.CreateWorkspace(&store.Workspace{ID: id, ClientID: c.ID, ProjectID: p.ID, Name: req.Name}); err != nil {
			return nil, internal(err)
		}
		wt, err := s.orca.CreateWorktree(ctx, p.Repo, req.Name)
		if err != nil {
			s.store.SetWorkspaceState(id, store.WorkspaceCreating, store.WorkspaceFailed, "", "", "", err.Error())
			return nil, orcaError(err)
		}
		if _, err := s.store.SetWorkspaceState(id, store.WorkspaceCreating, store.WorkspaceReady, wt.ID, wt.Path, wt.Branch, ""); err != nil {
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
func (s *Service) DeleteWorkspace(ctx context.Context, c *Client, req WorkspaceRequest) (*store.Operation, error) {
	rec := store.AuditRecord{ClientID: c.ID, Action: "delete_workspace", WorkspaceID: req.Workspace, OperationID: req.OperationID}
	w, err := s.workspace(c, req.Workspace)
	if err != nil {
		s.audit(rec, err)
		return nil, err
	}
	ctx = context.WithoutCancel(ctx)
	return s.operation(c, "delete_workspace", req.OperationID, req, rec, func() (any, error) {
		if busy, err := s.store.WorkspaceBusy(w.ID); err != nil {
			return nil, internal(err)
		} else if busy {
			return nil, errorf(CodeInvalidState, "workspace has running jobs or active agent sessions")
		}
		if ok, err := s.store.SetWorkspaceState(w.ID, store.WorkspaceReady, store.WorkspaceDeleting, "", "", "", ""); err != nil {
			return nil, internal(err)
		} else if !ok {
			return nil, errorf(CodeInvalidState, "workspace is not ready")
		}
		s.closeFS(w.ID)
		if err := s.orca.RemoveWorktree(ctx, w.OrcaID); err != nil {
			s.store.SetWorkspaceState(w.ID, store.WorkspaceDeleting, store.WorkspaceReady, "", "", "", err.Error())
			return nil, orcaError(err)
		}
		if _, err := s.store.SetWorkspaceState(w.ID, store.WorkspaceDeleting, store.WorkspaceDeleted, "", "", "", ""); err != nil {
			return nil, internal(err)
		}
		return s.workspace(c, w.ID)
	})
}

func (s *Service) workspace(c *Client, id string) (*store.Workspace, error) {
	w, err := s.store.GetWorkspace(c.ID, id)
	if errors.Is(err, store.ErrNotFound) {
		return nil, errorf(CodeNotFound, "workspace %q not found", id)
	}
	if err != nil {
		return nil, internal(err)
	}
	return w, nil
}

func orcaError(err error) error {
	var oe *orca.Error
	if errors.As(err, &oe) {
		return errorf(CodeOrcaFailed, "%s: %s", oe.Code, oe.Message)
	}
	return errorf(CodeOrcaFailed, "%v", err)
}

func randomHex() string {
	b := make([]byte, 12)
	rand.Read(b)
	return hex.EncodeToString(b)
}
