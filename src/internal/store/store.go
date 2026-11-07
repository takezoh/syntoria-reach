// Package store keeps Reach's records in SQLite: operations, audit
// records, jobs, agent sessions and workspaces.
package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"

	"github.com/takezoh/syntoria-reach/src/internal/core"
)

// Store is the SQLite-backed state of a Reach node.
type Store struct {
	db  *sql.DB
	now func() time.Time
}

// migrations are applied in order; PRAGMA user_version records how many ran.
var migrations = []string{
	`CREATE TABLE operations (
		client_id     TEXT NOT NULL,
		operation_id  TEXT NOT NULL,
		kind          TEXT NOT NULL,
		args_hash     TEXT NOT NULL,
		state         TEXT NOT NULL,
		result        BLOB,
		error_code    TEXT NOT NULL DEFAULT '',
		error_message TEXT NOT NULL DEFAULT '',
		created_at    INTEGER NOT NULL,
		updated_at    INTEGER NOT NULL,
		PRIMARY KEY (client_id, operation_id)
	);
	CREATE TABLE audit_records (
		id           INTEGER PRIMARY KEY,
		at           INTEGER NOT NULL,
		client_id    TEXT NOT NULL,
		action       TEXT NOT NULL,
		workspace_id TEXT NOT NULL DEFAULT '',
		target       TEXT NOT NULL DEFAULT '',
		operation_id TEXT NOT NULL DEFAULT '',
		outcome      TEXT NOT NULL,
		error_code   TEXT NOT NULL DEFAULT ''
	);`,
	`CREATE TABLE jobs (
		id            TEXT PRIMARY KEY,
		client_id     TEXT NOT NULL,
		workspace_id  TEXT NOT NULL,
		executable    TEXT NOT NULL,
		state         TEXT NOT NULL,
		exit_code     INTEGER,
		timed_out     INTEGER NOT NULL DEFAULT 0,
		truncated     INTEGER NOT NULL DEFAULT 0,
		error_message TEXT NOT NULL DEFAULT '',
		created_at    INTEGER NOT NULL,
		updated_at    INTEGER NOT NULL
	);
	CREATE TABLE agent_sessions (
		id                  TEXT PRIMARY KEY,
		client_id           TEXT NOT NULL,
		workspace_id        TEXT NOT NULL,
		agent_id            TEXT NOT NULL,
		protocol_session_id TEXT NOT NULL DEFAULT '',
		state               TEXT NOT NULL,
		turns               INTEGER NOT NULL DEFAULT 0,
		error_message       TEXT NOT NULL DEFAULT '',
		created_at          INTEGER NOT NULL,
		updated_at          INTEGER NOT NULL
	);
	CREATE TABLE agent_events (
		session_id TEXT NOT NULL,
		seq        INTEGER NOT NULL,
		turn       INTEGER NOT NULL,
		type       TEXT NOT NULL,
		text       TEXT NOT NULL,
		at         INTEGER NOT NULL,
		PRIMARY KEY (session_id, seq)
	);`,
	`CREATE TABLE workspaces (
		id            TEXT PRIMARY KEY,
		client_id     TEXT NOT NULL,
		project_id    TEXT NOT NULL,
		name          TEXT NOT NULL,
		state         TEXT NOT NULL,
		orca_id       TEXT NOT NULL DEFAULT '',
		path          TEXT NOT NULL DEFAULT '',
		branch        TEXT NOT NULL DEFAULT '',
		error_message TEXT NOT NULL DEFAULT '',
		created_at    INTEGER NOT NULL,
		updated_at    INTEGER NOT NULL
	);`,
}

// Open opens (creating if needed) the database at path and applies migrations.
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(FULL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// A single connection serialises writes and keeps admission atomic.
	db.SetMaxOpenConns(1)
	s := &Store{db: db, now: time.Now}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	var version int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return err
	}
	for i := version; i < len(migrations); i++ {
		tx, err := s.db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, i+1)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// Admit records a new running operation. If the client already used
// operationID, the existing record is returned with created=false and nothing
// is written; the caller compares ArgsHash to detect a conflict.
func (s *Store) Admit(clientID, operationID, kind, argsHash string) (op *core.Operation, created bool, err error) {
	now := s.now().UnixMilli()
	res, err := s.db.Exec(`INSERT INTO operations
		(client_id, operation_id, kind, args_hash, state, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (client_id, operation_id) DO NOTHING`,
		clientID, operationID, kind, argsHash, core.StateRunning, now, now)
	if err != nil {
		return nil, false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return nil, false, err
	}
	op, err = s.GetOperation(clientID, operationID)
	if err != nil {
		return nil, false, err
	}
	return op, n == 1, nil
}

// Finish stores the outcome of a running operation.
func (s *Store) Finish(clientID, operationID, state string, result any, errCode, errMessage string) (*core.Operation, error) {
	var raw []byte
	if result != nil {
		var err error
		if raw, err = json.Marshal(result); err != nil {
			return nil, err
		}
	}
	_, err := s.db.Exec(`UPDATE operations
		SET state = ?, result = ?, error_code = ?, error_message = ?, updated_at = ?
		WHERE client_id = ? AND operation_id = ? AND state = ?`,
		state, raw, errCode, errMessage, s.now().UnixMilli(), clientID, operationID, core.StateRunning)
	if err != nil {
		return nil, err
	}
	return s.GetOperation(clientID, operationID)
}

// GetOperation returns the client's operation.
func (s *Store) GetOperation(clientID, operationID string) (*core.Operation, error) {
	op := &core.Operation{ClientID: clientID, OperationID: operationID}
	var result []byte
	var created, updated int64
	err := s.db.QueryRow(`SELECT kind, args_hash, state, result, error_code, error_message, created_at, updated_at
		FROM operations WHERE client_id = ? AND operation_id = ?`, clientID, operationID).
		Scan(&op.Kind, &op.ArgsHash, &op.State, &result, &op.ErrorCode, &op.ErrorMessage, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, core.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if len(result) > 0 {
		op.Result = result
	}
	op.CreatedAt = time.UnixMilli(created).UTC()
	op.UpdatedAt = time.UnixMilli(updated).UTC()
	return op, nil
}

// MarkRunningUnknown marks operations, jobs, agent sessions and workspace
// changes left active by a previous daemon process as unknown. Their processes died with that daemon
// (or their fate cannot be confirmed), and they are never resumed. It must be
// called before serving requests and returns the number of records changed.
func (s *Store) MarkRunningUnknown() (int64, error) {
	const msg = "daemon stopped before the outcome was recorded"
	now := s.now().UnixMilli()
	var total int64
	for _, q := range []string{
		`UPDATE operations SET state = 'unknown', error_code = 'result_unknown', error_message = ?, updated_at = ? WHERE state = 'running'`,
		`UPDATE jobs SET state = 'unknown', error_message = ?, updated_at = ? WHERE state = 'running'`,
		`UPDATE agent_sessions SET state = 'unknown', error_message = ?, updated_at = ? WHERE state IN ('starting', 'idle', 'running')`,
		`UPDATE workspaces SET state = 'unknown', error_message = ?, updated_at = ? WHERE state IN ('creating', 'deleting')`,
	} {
		res, err := s.db.Exec(q, msg, now)
		if err != nil {
			return total, err
		}
		n, _ := res.RowsAffected()
		total += n
	}
	return total, nil
}

// Audit appends an audit record.
func (s *Store) Audit(r core.AuditRecord) error {
	_, err := s.db.Exec(`INSERT INTO audit_records
		(at, client_id, action, workspace_id, target, operation_id, outcome, error_code)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		s.now().UnixMilli(), r.ClientID, r.Action, r.WorkspaceID, r.Target, r.OperationID, r.Outcome, r.ErrorCode)
	return err
}
