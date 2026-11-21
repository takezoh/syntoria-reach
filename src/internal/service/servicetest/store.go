// Package servicetest provides in-memory fakes of the service ports and
// the contract tests that hold the fakes and the real adapters to the same
// behavior.
package servicetest

import (
	"encoding/json"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/takezoh/syntoria-reach/src/internal/core"
)

// Store is an in-memory service.Store. It returns copies, so callers never
// share state with it.
type Store struct {
	// Now is the clock; it defaults to time.Now.
	Now func() time.Time

	mu         sync.Mutex
	operations map[[2]string]*core.Operation // client ID, operation ID
	audit      []core.AuditRecord
	auditAt    map[string]time.Time // workspace ID -> time of its latest audit record
	turns      map[[2]string]turnRecord
	stops      []core.TurnStop
	jobs       map[string]*core.Job
}

// NewStore returns an empty store.
func NewStore() *Store {
	return &Store{
		operations: map[[2]string]*core.Operation{},
		auditAt:    map[string]time.Time{},
		jobs:       map[string]*core.Job{},
	}
}

func (s *Store) now() time.Time {
	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	return time.UnixMilli(now().UnixMilli()).UTC() // the precision SQLite keeps
}

// AuditRecords returns the audit records written so far.
func (s *Store) AuditRecords() []core.AuditRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.audit)
}

func (s *Store) Admit(clientID, operationID, kind, argsHash string) (*core.Operation, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := [2]string{clientID, operationID}
	if op, ok := s.operations[key]; ok {
		return copyOp(op), false, nil
	}
	now := s.now()
	op := &core.Operation{ClientID: clientID, OperationID: operationID, Kind: kind, ArgsHash: argsHash,
		State: core.StateRunning, CreatedAt: now, UpdatedAt: now}
	s.operations[key] = op
	return copyOp(op), true, nil
}

func (s *Store) Finish(clientID, operationID, state string, result any, errCode, errMessage string) (*core.Operation, error) {
	var raw json.RawMessage
	if result != nil {
		b, err := json.Marshal(result)
		if err != nil {
			return nil, err
		}
		raw = b
	}
	s.mu.Lock()
	op, ok := s.operations[[2]string{clientID, operationID}]
	if ok && op.State == core.StateRunning {
		op.State, op.Result, op.ErrorCode, op.ErrorMessage, op.UpdatedAt = state, raw, errCode, errMessage, s.now()
		if v, ok := s.turns[[2]string{clientID, operationID}]; ok && !v.armed && state == core.StateFailed {
			delete(s.turns, [2]string{clientID, operationID})
		}
	}
	s.mu.Unlock()
	return s.GetOperation(clientID, operationID)
}

func (s *Store) GetOperation(clientID, operationID string) (*core.Operation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	op, ok := s.operations[[2]string{clientID, operationID}]
	if !ok {
		return nil, core.ErrNotFound
	}
	return copyOp(op), nil
}

func copyOp(op *core.Operation) *core.Operation {
	c := *op
	c.Result = slices.Clone(op.Result)
	return &c
}

func (s *Store) Audit(r core.AuditRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.audit = append(s.audit, r)
	if r.WorkspaceID != "" {
		s.auditAt[r.WorkspaceID] = s.now()
	}
	return nil
}

func (s *Store) WorkspaceLastUsed(id string) (time.Time, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	last := s.auditAt[id]
	for _, j := range s.jobs {
		if j.WorkspaceID == id && j.UpdatedAt.After(last) {
			last = j.UpdatedAt
		}
	}
	return last, nil
}

func (s *Store) WorkspaceBusy(id string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, j := range s.jobs {
		if j.WorkspaceID == id && j.State == core.JobRunning {
			return true, nil
		}
	}
	return false, nil
}

func (s *Store) CreateJob(j *core.Job) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.jobs[j.ID]; ok {
		return fmt.Errorf("job %s exists", j.ID)
	}
	now := s.now()
	s.jobs[j.ID] = &core.Job{ID: j.ID, ClientID: j.ClientID, WorkspaceID: j.WorkspaceID, Executable: j.Executable,
		State: core.JobRunning, CreatedAt: now, UpdatedAt: now}
	return nil
}

func (s *Store) FinishJob(id, state string, exitCode *int, timedOut, truncated bool, errMessage string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.jobs[id]
	if !ok || j.State != core.JobRunning {
		return nil
	}
	j.State, j.TimedOut, j.Truncated, j.ErrorMessage, j.UpdatedAt = state, timedOut, truncated, errMessage, s.now()
	j.ExitCode = nil
	if exitCode != nil {
		code := *exitCode
		j.ExitCode = &code
	}
	return nil
}

func (s *Store) GetJob(clientID, id string) (*core.Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.jobs[id]
	if !ok || j.ClientID != clientID {
		return nil, core.ErrNotFound
	}
	c := *j
	if j.ExitCode != nil {
		code := *j.ExitCode
		c.ExitCode = &code
	}
	return &c, nil
}

func (s *Store) BeginTurn(t core.Turn) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.turns == nil {
		s.turns = map[[2]string]turnRecord{}
	}
	k := [2]string{t.ClientID, t.OperationID}
	if _, ok := s.turns[k]; ok {
		return fmt.Errorf("turn exists")
	}
	s.turns[k] = turnRecord{turn: t}
	return nil
}
func (s *Store) ArmTurn(t core.Turn) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := [2]string{t.ClientID, t.OperationID}
	if v, ok := s.turns[k]; ok && v.turn == t {
		v.armed = true
		s.turns[k] = v
		return nil
	}
	return fmt.Errorf("pending turn not found")
}
func (s *Store) ArmedTurns() ([]core.Turn, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []core.Turn
	for _, v := range s.turns {
		if v.armed {
			out = append(out, v.turn)
		}
	}
	return out, nil
}
func (s *Store) CloseTurn(t core.Turn, stop core.TurnStop) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := [2]string{t.ClientID, t.OperationID}
	if v, ok := s.turns[k]; ok && v.turn == t && v.armed {
		for _, e := range s.stops {
			if e.EventID == stop.EventID {
				return fmt.Errorf("event exists")
			}
		}
		delete(s.turns, k)
		s.stops = append(s.stops, stop)
	}
	return nil
}
func (s *Store) DisarmTurn(t core.Turn) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := [2]string{t.ClientID, t.OperationID}
	if v, ok := s.turns[k]; ok && v.turn == t && v.armed {
		delete(s.turns, k)
	}
	return nil
}
func (s *Store) PendingTurnStops() ([]core.TurnStop, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.stops), nil
}
func (s *Store) DeleteTurnStop(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stops = slices.DeleteFunc(s.stops, func(e core.TurnStop) bool { return e.EventID == id })
	return nil
}

type turnRecord struct {
	turn  core.Turn
	armed bool
}

func (s *Store) PendingTurns() ([]core.Turn, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []core.Turn
	for _, v := range s.turns {
		if !v.armed {
			out = append(out, v.turn)
		}
	}
	return out, nil
}
func (s *Store) ClosePendingTurn(t core.Turn, stop core.TurnStop) error {
	return s.reconcilePendingTurn(t, stop, false)
}
func (s *Store) ArmPendingTurn(t core.Turn, stop core.TurnStop) error {
	return s.reconcilePendingTurn(t, stop, true)
}
func (s *Store) reconcilePendingTurn(t core.Turn, stop core.TurnStop, arm bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := [2]string{t.ClientID, t.OperationID}
	if v, ok := s.turns[k]; ok && v.turn == t && !v.armed {
		for _, e := range s.stops {
			if e.EventID == stop.EventID {
				return fmt.Errorf("duplicate event ID")
			}
		}
		if arm {
			v.armed = true
			s.turns[k] = v
		} else {
			delete(s.turns, k)
		}
		s.stops = append(s.stops, stop)
	}
	return nil
}
