package store

import (
	"database/sql"
	"errors"
	"time"

	"github.com/takezoh/syntoria-reach/src/internal/core"
)

// CreateJob records a new running job.
func (s *Store) CreateJob(j *core.Job) error {
	now := s.now().UnixMilli()
	_, err := s.db.Exec(`INSERT INTO jobs (id, client_id, workspace_id, executable, state, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, j.ID, j.ClientID, j.WorkspaceID, j.Executable, core.JobRunning, now, now)
	return err
}

// FinishJob stores the outcome of a running job. A job that is no longer
// running (e.g. marked unknown) is left unchanged.
func (s *Store) FinishJob(id, state string, exitCode *int, timedOut, truncated bool, errMessage string) error {
	_, err := s.db.Exec(`UPDATE jobs SET state = ?, exit_code = ?, timed_out = ?, truncated = ?, error_message = ?, updated_at = ?
		WHERE id = ? AND state = ?`, state, exitCode, timedOut, truncated, errMessage, s.now().UnixMilli(), id, core.JobRunning)
	return err
}

// GetJob returns the client's job.
func (s *Store) GetJob(clientID, id string) (*core.Job, error) {
	j := &core.Job{ID: id, ClientID: clientID}
	var exit sql.NullInt64
	var created, updated int64
	err := s.db.QueryRow(`SELECT workspace_id, executable, state, exit_code, timed_out, truncated, error_message, created_at, updated_at
		FROM jobs WHERE id = ? AND client_id = ?`, id, clientID).
		Scan(&j.WorkspaceID, &j.Executable, &j.State, &exit, &j.TimedOut, &j.Truncated, &j.ErrorMessage, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, core.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if exit.Valid {
		code := int(exit.Int64)
		j.ExitCode = &code
	}
	j.CreatedAt, j.UpdatedAt = time.UnixMilli(created).UTC(), time.UnixMilli(updated).UTC()
	return j, nil
}

// CreateAgentSession records a new starting session.
func (s *Store) CreateAgentSession(a *core.AgentSession) error {
	now := s.now().UnixMilli()
	_, err := s.db.Exec(`INSERT INTO agent_sessions (id, client_id, workspace_id, agent_id, state, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, a.ID, a.ClientID, a.WorkspaceID, a.AgentID, core.SessionStarting, now, now)
	return err
}

// UpdateAgentSession applies u to a session that has not ended (closed,
// failed or unknown sessions are never changed).
func (s *Store) UpdateAgentSession(id string, u core.AgentSessionUpdate) error {
	turn := 0
	if u.AddTurn {
		turn = 1
	}
	_, err := s.db.Exec(`UPDATE agent_sessions SET
		state = COALESCE(NULLIF(?, ''), state),
		protocol_session_id = COALESCE(NULLIF(?, ''), protocol_session_id),
		error_message = COALESCE(NULLIF(?, ''), error_message),
		turns = turns + ?, updated_at = ?
		WHERE id = ? AND state IN ('starting', 'idle', 'running')`,
		u.State, u.ProtocolSessionID, u.ErrorMessage, turn, s.now().UnixMilli(), id)
	return err
}

// GetAgentSession returns the client's session.
func (s *Store) GetAgentSession(clientID, id string) (*core.AgentSession, error) {
	a := &core.AgentSession{ID: id, ClientID: clientID}
	var created, updated int64
	err := s.db.QueryRow(`SELECT workspace_id, agent_id, protocol_session_id, state, turns, error_message, created_at, updated_at
		FROM agent_sessions WHERE id = ? AND client_id = ?`, id, clientID).
		Scan(&a.WorkspaceID, &a.AgentID, &a.ProtocolSessionID, &a.State, &a.Turns, &a.ErrorMessage, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, core.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	a.CreatedAt, a.UpdatedAt = time.UnixMilli(created).UTC(), time.UnixMilli(updated).UTC()
	return a, nil
}

// AppendAgentEvent appends an event with the next sequence number.
func (s *Store) AppendAgentEvent(sessionID string, turn int, typ, text string) error {
	_, err := s.db.Exec(`INSERT INTO agent_events (session_id, seq, turn, type, text, at)
		SELECT ?, COALESCE(MAX(seq), 0) + 1, ?, ?, ?, ? FROM agent_events WHERE session_id = ?`,
		sessionID, turn, typ, text, s.now().UnixMilli(), sessionID)
	return err
}

// ListAgentEvents returns up to limit events with seq > after.
func (s *Store) ListAgentEvents(sessionID string, after int64, limit int) ([]core.AgentEvent, error) {
	rows, err := s.db.Query(`SELECT seq, turn, type, text, at FROM agent_events
		WHERE session_id = ? AND seq > ? ORDER BY seq LIMIT ?`, sessionID, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	events := []core.AgentEvent{}
	for rows.Next() {
		var e core.AgentEvent
		var at int64
		if err := rows.Scan(&e.Seq, &e.Turn, &e.Type, &e.Text, &at); err != nil {
			return nil, err
		}
		e.At = time.UnixMilli(at).UTC()
		events = append(events, e)
	}
	return events, rows.Err()
}
