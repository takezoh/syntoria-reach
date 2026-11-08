package service

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/takezoh/syntoria-reach/src/internal/core"
)

// A workspace is one unit of work: a folder of its own named with
// core.WorkspacePrefix, with a note saying what the work is and a status.
// The node's workspace backend keeps it (Orca, or Reach's own records of
// local workspaces). Completed workspaces are removed by Cleanup once idle
// for core.CleanupAfter.

// Capabilities is what the client may do in any workspace.
type Capabilities struct {
	Permissions []string         `json:"permissions"`
	Commands    []string         `json:"commands,omitempty"`
	Agents      []core.AgentInfo `json:"agents,omitempty"`
}

// ListCapabilities returns the client's permissions, with the commands and
// agents (and their roles) it may run when it holds the permission to run
// them.
func (s *Service) ListCapabilities(c *Client) Capabilities {
	out := Capabilities{Permissions: slices.Clone(c.perms)}
	if out.Permissions == nil {
		out.Permissions = []string{}
	}
	if slices.Contains(c.perms, core.PermCommandRun) {
		out.Commands = s.commands
	}
	if slices.Contains(c.perms, core.PermAgentRun) {
		for _, a := range s.agents {
			out.Agents = append(out.Agents, core.AgentInfo{ID: a.ID, Role: a.Role})
		}
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
	list, err := s.workspaces.List(ctx)
	if err != nil {
		return nil, classify(err)
	}
	out = append(out, list...)
	slices.SortFunc(out, func(a, b *core.Workspace) int { return cmp.Or(cmp.Compare(a.Name, b.Name), cmp.Compare(a.ID, b.ID)) })
	return out, nil
}

// CreateWorkspaceRequest creates a workspace for one new unit of work. Name
// becomes the folder name (and on Orca the project name), prefixed with
// core.WorkspacePrefix; Note says what the work is.
type CreateWorkspaceRequest struct {
	OperationID string `json:"operation_id"`
	Name        string `json:"name"`
	Note        string `json:"note"`
}

// CreateWorkspace creates the workspace, its folder and its note, once per
// operation ID. The result is the workspace.
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
		w, err := s.workspaces.Create(ctx, name, req.Note)
		if err != nil {
			return nil, err
		}
		return w, nil
	})
}

// UpdateWorkspaceRequest changes a workspace's note, its status (todo,
// in-progress, in-review, completed, or on Orca a custom one), or both.
type UpdateWorkspaceRequest struct {
	OperationID string `json:"operation_id"`
	Workspace   string `json:"workspace"`
	Note        string `json:"note"`
	Status      string `json:"status"`
}

// UpdateWorkspace updates a workspace once per operation ID. The result is
// the workspace.
func (s *Service) UpdateWorkspace(ctx context.Context, c *Client, req UpdateWorkspaceRequest) (*core.Operation, error) {
	rec := core.AuditRecord{ClientID: c.ID, Action: "update_workspace", WorkspaceID: req.Workspace, OperationID: req.OperationID}
	var err error
	switch {
	case !slices.Contains(c.perms, core.PermWorkspaceCreate):
		err = core.Errorf(core.CodePermissionDenied, "%s is not allowed", core.PermWorkspaceCreate)
	case req.Note == "" && req.Status == "":
		err = core.Errorf(core.CodeInvalidRequest, "set note, status or both")
	case req.Status != "" && !core.ValidWorkspaceStatus(req.Status):
		err = core.Errorf(core.CodeInvalidRequest, "status must be a workspace status such as todo, in-progress, in-review or completed")
	case req.Note != "":
		err = core.CheckNote(req.Note)
	}
	if err != nil {
		s.audit(rec, err)
		return nil, err
	}
	ctx = context.WithoutCancel(ctx)
	return s.operation(c, "update_workspace", req.OperationID, req, rec, func() (any, error) {
		w, err := s.workspaces.Update(ctx, req.Workspace, req.Note, req.Status)
		switch {
		case errors.Is(err, core.ErrNotFound):
			return nil, workspaceNotFound(req.Workspace)
		case err != nil:
			return nil, err
		}
		return w, nil
	})
}

// reopen sets a completed workspace back in progress: work resumed in it.
func (s *Service) reopen(ctx context.Context, w *core.Workspace) {
	if w.Status != core.WorkspaceCompleted {
		return
	}
	if _, err := s.workspaces.Update(ctx, w.ID, "", core.WorkspaceInProgress); err != nil {
		slog.Warn("cannot set the workspace in progress", "workspace", w.ID, "err", err)
	}
}

// Cleanup removes the workspaces that are completed and have had no
// activity, in the backend or through Reach, for core.CleanupAfter, with
// the folders Reach created for them. It returns the IDs of the workspaces
// removed. It also has the backend sweep the folders Reach created that no
// workspace has any more, such as those of projects removed in Orca, once
// nothing in them changed for core.CleanupAfter.
func (s *Service) Cleanup(ctx context.Context, now time.Time) ([]string, error) {
	removed, err := s.cleanupWorkspaces(ctx, now)
	swept, serr := s.workspaces.Sweep(ctx, now.Add(-core.CleanupAfter))
	for _, name := range swept {
		s.audit(core.AuditRecord{ClientID: "reach", Action: "cleanup_folder", Target: name}, nil)
		slog.Info("removed the folder of a workspace that is gone", "folder", name)
	}
	return removed, errors.Join(err, serr)
}

// cleanupWorkspaces removes the completed workspaces that are due.
func (s *Service) cleanupWorkspaces(ctx context.Context, now time.Time) ([]string, error) {
	list, err := s.workspaces.List(ctx)
	if err != nil {
		return nil, err
	}
	var removed []string
	var errs []error
	var open map[string]bool // workspaces with an open session
	for _, w := range list {
		if w.Status != core.WorkspaceCompleted {
			continue
		}
		if open == nil {
			if open, err = s.workspaces.Busy(ctx); err != nil {
				return nil, err
			}
		}
		if done, err := s.cleanup(ctx, w, open[w.ID], now); err != nil {
			errs = append(errs, fmt.Errorf("workspace %s: %w", w.ID, err))
		} else if done {
			removed = append(removed, w.ID)
		}
	}
	return removed, errors.Join(errs...)
}

// cleanup removes one completed workspace if it is due, and reports whether
// it was removed. An open session, such as an agent's terminal, makes it
// busy.
func (s *Service) cleanup(ctx context.Context, w *core.Workspace, sessionOpen bool, now time.Time) (bool, error) {
	// A workspace a request holds is skipped; once marked, no request can
	// take it, and the jobs and sessions of earlier requests are recorded.
	if !s.beginRemoval(w.ID) {
		return false, nil
	}
	defer s.endRemoval(w.ID)
	busy, err := s.store.WorkspaceBusy(w.ID)
	if err != nil {
		return false, err
	}
	busy = busy || sessionOpen
	used, err := s.store.WorkspaceLastUsed(w.ID)
	if err != nil {
		return false, err
	}
	last := w.LastActivity
	if used.After(last) {
		last = used
	}
	if !core.Expired(w.Status, last, now, busy) {
		return false, nil
	}
	s.closeFiles(w.ID)
	if err := s.workspaces.Remove(ctx, w); err != nil {
		return false, err
	}
	s.audit(core.AuditRecord{ClientID: "reach", Action: "cleanup_workspace", WorkspaceID: w.ID, Target: w.Name}, nil)
	return true, nil
}

// workspace reads workspace id from the backend.
func (s *Service) workspace(ctx context.Context, id string) (*core.Workspace, error) {
	w, err := s.workspaces.Get(ctx, id)
	if errors.Is(err, core.ErrNotFound) {
		return nil, workspaceNotFound(id)
	}
	if err != nil {
		return nil, classify(err)
	}
	return w, nil
}

func workspaceNotFound(id string) error {
	return core.Errorf(core.CodeNotFound, "workspace %q not found", id)
}

// use holds workspace id for a request until the returned release is
// called. It fails while cleanup is removing the workspace.
func (s *Service) use(id string) (release func(), ok bool) {
	s.useMu.Lock()
	defer s.useMu.Unlock()
	if s.removing[id] {
		return nil, false
	}
	s.users[id]++
	var once sync.Once
	return func() {
		once.Do(func() {
			s.useMu.Lock()
			defer s.useMu.Unlock()
			if s.users[id]--; s.users[id] == 0 {
				delete(s.users, id)
			}
		})
	}, true
}

// beginRemoval marks workspace id as being removed, unless a request holds
// it or it is marked already.
func (s *Service) beginRemoval(id string) bool {
	s.useMu.Lock()
	defer s.useMu.Unlock()
	if s.removing[id] || s.users[id] > 0 {
		return false
	}
	s.removing[id] = true
	return true
}

func (s *Service) endRemoval(id string) {
	s.useMu.Lock()
	defer s.useMu.Unlock()
	delete(s.removing, id)
}
