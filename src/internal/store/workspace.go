package store

import (
	"database/sql"
	"time"
)

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

// WorkspaceLastUsed returns when Reach last acted on a workspace: its latest
// audit record, agent session change or job change; the zero time if none.
func (s *Store) WorkspaceLastUsed(id string) (time.Time, error) {
	var at sql.NullInt64
	err := s.db.QueryRow(`SELECT MAX(t) FROM (
		SELECT MAX(at) AS t FROM audit_records WHERE workspace_id = ?
		UNION ALL SELECT MAX(updated_at) FROM agent_sessions WHERE workspace_id = ?
		UNION ALL SELECT MAX(updated_at) FROM jobs WHERE workspace_id = ?)`, id, id, id).Scan(&at)
	if err != nil || !at.Valid {
		return time.Time{}, err
	}
	return time.UnixMilli(at.Int64).UTC(), nil
}
