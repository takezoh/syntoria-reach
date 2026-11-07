package store

import (
	"database/sql"
	"errors"
	"time"

	"github.com/takezoh/syntoria-reach/src/internal/core"
)

// CreateWorkspace records a workspace in the creating state.
func (s *Store) CreateWorkspace(w *core.Workspace) error {
	now := s.now().UnixMilli()
	_, err := s.db.Exec(`INSERT INTO workspaces (id, client_id, project_id, name, state, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, w.ID, w.ClientID, w.ProjectID, w.Name, core.WorkspaceCreating, now, now)
	return err
}

// SetWorkspaceState moves a workspace from one state to another and reports
// whether it was in the from state. Empty orcaID, path and branch are left
// unchanged.
func (s *Store) SetWorkspaceState(id, from, to, orcaID, path, branch, errMessage string) (bool, error) {
	res, err := s.db.Exec(`UPDATE workspaces SET state = ?,
		orca_id = COALESCE(NULLIF(?, ''), orca_id),
		path = COALESCE(NULLIF(?, ''), path),
		branch = COALESCE(NULLIF(?, ''), branch),
		error_message = ?, updated_at = ?
		WHERE id = ? AND state = ?`, to, orcaID, path, branch, errMessage, s.now().UnixMilli(), id, from)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

const workspaceColumns = `id, client_id, project_id, name, state, orca_id, path, branch, error_message, created_at, updated_at`

func scanWorkspace(row interface{ Scan(...any) error }) (*core.Workspace, error) {
	w := &core.Workspace{}
	var created, updated int64
	err := row.Scan(&w.ID, &w.ClientID, &w.ProjectID, &w.Name, &w.State, &w.OrcaID, &w.Path, &w.Branch, &w.ErrorMessage, &created, &updated)
	if err != nil {
		return nil, err
	}
	w.CreatedAt, w.UpdatedAt = time.UnixMilli(created).UTC(), time.UnixMilli(updated).UTC()
	return w, nil
}

// GetWorkspace returns the client's workspace.
func (s *Store) GetWorkspace(clientID, id string) (*core.Workspace, error) {
	w, err := scanWorkspace(s.db.QueryRow(`SELECT `+workspaceColumns+` FROM workspaces WHERE id = ? AND client_id = ?`, id, clientID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, core.ErrNotFound
	}
	return w, err
}

// ListWorkspaces returns the client's ready workspaces, oldest first.
func (s *Store) ListWorkspaces(clientID string) ([]*core.Workspace, error) {
	rows, err := s.db.Query(`SELECT `+workspaceColumns+` FROM workspaces
		WHERE client_id = ? AND state = ? ORDER BY created_at, id`, clientID, core.WorkspaceReady)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*core.Workspace{}
	for rows.Next() {
		w, err := scanWorkspace(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// WorkspaceBusy reports whether a workspace has running jobs or active agent
// sessions.
func (s *Store) WorkspaceBusy(id string) (bool, error) {
	var n int
	err := s.db.QueryRow(`SELECT
		(SELECT COUNT(*) FROM jobs WHERE workspace_id = ? AND state = 'running') +
		(SELECT COUNT(*) FROM agent_sessions WHERE workspace_id = ? AND state IN ('starting', 'idle', 'running'))`,
		id, id).Scan(&n)
	return n > 0, err
}
