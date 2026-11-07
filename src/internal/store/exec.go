package store

import (
	"database/sql"
	"errors"
	"time"
)

// Job states. Running jobs left by a previous daemon become StateUnknown.
const (
	JobRunning   = "running"
	JobSucceeded = "succeeded"
	JobFailed    = "failed"
	JobCancelled = "cancelled"
)

// Job is the durable record of a background command.
type Job struct {
	ID           string    `json:"job_id"`
	ClientID     string    `json:"-"`
	WorkspaceID  string    `json:"workspace"`
	Executable   string    `json:"executable"`
	State        string    `json:"state"`
	ExitCode     *int      `json:"exit_code,omitempty"`
	TimedOut     bool      `json:"timed_out"`
	Truncated    bool      `json:"truncated"`
	ErrorMessage string    `json:"error_message,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// CreateJob records a new running job.
func (s *Store) CreateJob(j *Job) error {
	now := s.now().UnixMilli()
	_, err := s.db.Exec(`INSERT INTO jobs (id, client_id, workspace_id, executable, state, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, j.ID, j.ClientID, j.WorkspaceID, j.Executable, JobRunning, now, now)
	return err
}

// FinishJob stores the outcome of a running job. A job that is no longer
// running (e.g. marked unknown) is left unchanged.
func (s *Store) FinishJob(id, state string, exitCode *int, timedOut, truncated bool, errMessage string) error {
	_, err := s.db.Exec(`UPDATE jobs SET state = ?, exit_code = ?, timed_out = ?, truncated = ?, error_message = ?, updated_at = ?
		WHERE id = ? AND state = ?`, state, exitCode, timedOut, truncated, errMessage, s.now().UnixMilli(), id, JobRunning)
	return err
}

// GetJob returns the client's job.
func (s *Store) GetJob(clientID, id string) (*Job, error) {
	j := &Job{ID: id, ClientID: clientID}
	var exit sql.NullInt64
	var created, updated int64
	err := s.db.QueryRow(`SELECT workspace_id, executable, state, exit_code, timed_out, truncated, error_message, created_at, updated_at
		FROM jobs WHERE id = ? AND client_id = ?`, id, clientID).
		Scan(&j.WorkspaceID, &j.Executable, &j.State, &exit, &j.TimedOut, &j.Truncated, &j.ErrorMessage, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
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

// Agent session states. Active sessions left by a previous daemon become
// StateUnknown.
const (
	SessionStarting = "starting"
	SessionIdle     = "idle"
	SessionRunning  = "running"
	SessionClosed   = "closed"
	SessionFailed   = "failed"
)

// AgentSession is the durable record of an agent harness session.
type AgentSession struct {
	ID                string    `json:"session_id"`
	ClientID          string    `json:"-"`
	WorkspaceID       string    `json:"workspace"`
	AgentID           string    `json:"agent"`
	ProtocolSessionID string    `json:"protocol_session_id,omitempty"`
	State             string    `json:"state"`
	Turns             int       `json:"turns"`
	ErrorMessage      string    `json:"error_message,omitempty"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
}

// AgentEvent is one normalized event of an agent session.
type AgentEvent struct {
	Seq  int64     `json:"seq"`
	Turn int       `json:"turn"`
	Type string    `json:"type"`
	Text string    `json:"text"`
	At   time.Time `json:"at"`
}

// CreateAgentSession records a new starting session.
func (s *Store) CreateAgentSession(a *AgentSession) error {
	now := s.now().UnixMilli()
	_, err := s.db.Exec(`INSERT INTO agent_sessions (id, client_id, workspace_id, agent_id, state, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, a.ID, a.ClientID, a.WorkspaceID, a.AgentID, SessionStarting, now, now)
	return err
}

// AgentSessionUpdate changes a session; empty fields are left unchanged.
type AgentSessionUpdate struct {
	State             string
	ProtocolSessionID string
	ErrorMessage      string
	AddTurn           bool
}

// UpdateAgentSession applies u to a session that has not ended (closed,
// failed or unknown sessions are never changed).
func (s *Store) UpdateAgentSession(id string, u AgentSessionUpdate) error {
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
func (s *Store) GetAgentSession(clientID, id string) (*AgentSession, error) {
	a := &AgentSession{ID: id, ClientID: clientID}
	var created, updated int64
	err := s.db.QueryRow(`SELECT workspace_id, agent_id, protocol_session_id, state, turns, error_message, created_at, updated_at
		FROM agent_sessions WHERE id = ? AND client_id = ?`, id, clientID).
		Scan(&a.WorkspaceID, &a.AgentID, &a.ProtocolSessionID, &a.State, &a.Turns, &a.ErrorMessage, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
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
func (s *Store) ListAgentEvents(sessionID string, after int64, limit int) ([]AgentEvent, error) {
	rows, err := s.db.Query(`SELECT seq, turn, type, text, at FROM agent_events
		WHERE session_id = ? AND seq > ? ORDER BY seq LIMIT ?`, sessionID, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	events := []AgentEvent{}
	for rows.Next() {
		var e AgentEvent
		var at int64
		if err := rows.Scan(&e.Seq, &e.Turn, &e.Type, &e.Text, &at); err != nil {
			return nil, err
		}
		e.At = time.UnixMilli(at).UTC()
		events = append(events, e)
	}
	return events, rows.Err()
}
