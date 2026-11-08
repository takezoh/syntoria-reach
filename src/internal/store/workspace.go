package store

import (
	"database/sql"
	"errors"
	"time"

	"github.com/takezoh/syntoria-reach/src/internal/core"
)

// WorkspaceBusy reports whether a workspace has running jobs.
func (s *Store) WorkspaceBusy(id string) (bool, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM jobs WHERE workspace_id = ? AND state = 'running'`, id).Scan(&n)
	return n > 0, err
}

// WorkspaceLastUsed returns when Reach last acted on a workspace: its latest
// audit record or job change; the zero time if none.
func (s *Store) WorkspaceLastUsed(id string) (time.Time, error) {
	var at sql.NullInt64
	err := s.db.QueryRow(`SELECT MAX(t) FROM (
		SELECT MAX(at) AS t FROM audit_records WHERE workspace_id = ?
		UNION ALL SELECT MAX(updated_at) FROM jobs WHERE workspace_id = ?)`, id, id).Scan(&at)
	if err != nil || !at.Valid {
		return time.Time{}, err
	}
	return time.UnixMilli(at.Int64).UTC(), nil
}

// CreateWorkspace records a local workspace; its last activity is now.
func (s *Store) CreateWorkspace(w *core.Workspace) error {
	now := s.now().UnixMilli()
	_, err := s.db.Exec(`INSERT INTO local_workspaces (id, name, path, note, status, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, w.ID, w.Name, w.Path, w.Note, w.Status, now, now)
	return err
}

// Workspaces returns every local workspace.
func (s *Store) Workspaces() ([]*core.Workspace, error) {
	rows, err := s.db.Query(`SELECT id, name, path, note, status, updated_at FROM local_workspaces ORDER BY name, id`)
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

// Workspace returns one local workspace; core.ErrNotFound if there is none.
func (s *Store) Workspace(id string) (*core.Workspace, error) {
	w, err := scanWorkspace(s.db.QueryRow(`SELECT id, name, path, note, status, updated_at FROM local_workspaces WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, core.ErrNotFound
	}
	return w, err
}

// UpdateWorkspace changes a local workspace's note and status (an empty
// value leaves that one unchanged) and makes now its last activity.
func (s *Store) UpdateWorkspace(id, note, status string) (*core.Workspace, error) {
	res, err := s.db.Exec(`UPDATE local_workspaces
		SET note = CASE WHEN ? = '' THEN note ELSE ? END, status = CASE WHEN ? = '' THEN status ELSE ? END, updated_at = ?
		WHERE id = ?`, note, note, status, status, s.now().UnixMilli(), id)
	if err != nil {
		return nil, err
	}
	if n, err := res.RowsAffected(); err != nil {
		return nil, err
	} else if n == 0 {
		return nil, core.ErrNotFound
	}
	return s.Workspace(id)
}

// DeleteWorkspace forgets a local workspace.
func (s *Store) DeleteWorkspace(id string) error {
	_, err := s.db.Exec(`DELETE FROM local_workspaces WHERE id = ?`, id)
	return err
}

func scanWorkspace(row interface{ Scan(...any) error }) (*core.Workspace, error) {
	w := &core.Workspace{}
	var updated int64
	if err := row.Scan(&w.ID, &w.Name, &w.Path, &w.Note, &w.Status, &updated); err != nil {
		return nil, err
	}
	w.LastActivity = time.UnixMilli(updated).UTC()
	return w, nil
}
