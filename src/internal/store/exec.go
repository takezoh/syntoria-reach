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
