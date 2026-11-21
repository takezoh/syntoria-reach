package store

import (
	"fmt"
	"time"

	"github.com/takezoh/syntoria-reach/src/internal/core"
)

func (s *Store) BeginTurn(t core.Turn) error {
	_, err := s.db.Exec(`INSERT INTO reach_turns(client_id,operation_id,session_id) VALUES(?,?,?)`, t.ClientID, t.OperationID, t.SessionID)
	return err
}
func (s *Store) ArmTurn(t core.Turn) error {
	res, err := s.db.Exec(`UPDATE reach_turns SET armed=1 WHERE client_id=? AND operation_id=? AND session_id=?`, t.ClientID, t.OperationID, t.SessionID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("pending turn not found")
	}
	return nil
}
func (s *Store) ArmedTurns() ([]core.Turn, error) {
	rows, err := s.db.Query(`SELECT client_id,operation_id,session_id FROM reach_turns WHERE armed=1 ORDER BY rowid`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ts []core.Turn
	for rows.Next() {
		var t core.Turn
		if err := rows.Scan(&t.ClientID, &t.OperationID, &t.SessionID); err != nil {
			return nil, err
		}
		ts = append(ts, t)
	}
	return ts, rows.Err()
}
func (s *Store) CloseTurn(t core.Turn, stop core.TurnStop) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`DELETE FROM reach_turns WHERE client_id=? AND operation_id=? AND session_id=? AND armed=1`, t.ClientID, t.OperationID, t.SessionID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 1 {
		_, err = tx.Exec(`INSERT INTO turn_stops(event_id,session_id,state,observed_at) VALUES(?,?,?,?)`, stop.EventID, stop.SessionID, stop.State, stop.ObservedAt.UTC().Format(time.RFC3339Nano))
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

// DisarmTurn removes an armed turn whose session is no longer observable.
func (s *Store) DisarmTurn(t core.Turn) error {
	_, err := s.db.Exec(`DELETE FROM reach_turns WHERE client_id=? AND operation_id=? AND session_id=? AND armed=1`, t.ClientID, t.OperationID, t.SessionID)
	return err
}
func (s *Store) PendingTurnStops() ([]core.TurnStop, error) {
	rows, err := s.db.Query(`SELECT event_id,session_id,state,observed_at FROM turn_stops ORDER BY rowid`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var stops []core.TurnStop
	for rows.Next() {
		var s core.TurnStop
		var at string
		if err := rows.Scan(&s.EventID, &s.SessionID, &s.State, &at); err != nil {
			return nil, err
		}
		s.ObservedAt, err = time.Parse(time.RFC3339Nano, at)
		if err != nil {
			return nil, err
		}
		stops = append(stops, s)
	}
	return stops, rows.Err()
}
func (s *Store) DeleteTurnStop(id string) error {
	_, err := s.db.Exec(`DELETE FROM turn_stops WHERE event_id=?`, id)
	return err
}

// PendingTurns lists unarmed intents left by a previous daemon.
func (s *Store) PendingTurns() ([]core.Turn, error) {
	rows, err := s.db.Query(`SELECT client_id,operation_id,session_id FROM reach_turns WHERE armed=0 ORDER BY rowid`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var turns []core.Turn
	for rows.Next() {
		var t core.Turn
		if err := rows.Scan(&t.ClientID, &t.OperationID, &t.SessionID); err != nil {
			return nil, err
		}
		turns = append(turns, t)
	}
	return turns, rows.Err()
}

// ClosePendingTurn atomically replaces an unarmed intent with its unknown occurrence.
func (s *Store) ClosePendingTurn(t core.Turn, stop core.TurnStop) error {
	return s.reconcilePendingTurn(`DELETE FROM reach_turns WHERE client_id=? AND operation_id=? AND session_id=? AND armed=0`, t, stop)
}

// ArmPendingTurn atomically arms an unarmed intent and records its unknown occurrence.
func (s *Store) ArmPendingTurn(t core.Turn, stop core.TurnStop) error {
	return s.reconcilePendingTurn(`UPDATE reach_turns SET armed=1 WHERE client_id=? AND operation_id=? AND session_id=? AND armed=0`, t, stop)
}

func (s *Store) reconcilePendingTurn(query string, t core.Turn, stop core.TurnStop) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.Exec(query, t.ClientID, t.OperationID, t.SessionID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 1 {
		_, err = tx.Exec(`INSERT INTO turn_stops(event_id,session_id,state,observed_at) VALUES(?,?,?,?)`, stop.EventID, stop.SessionID, stop.State, stop.ObservedAt.UTC().Format(time.RFC3339Nano))
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}
