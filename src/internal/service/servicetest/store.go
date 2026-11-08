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
