// Package servicetest provides in-memory fakes of the service ports and
// the contract tests that hold the fakes and the real adapters to the same
// behavior.
package servicetest

import (
	"cmp"
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
	sessions   map[string]*core.AgentSession
	events     map[string][]core.AgentEvent
}

// NewStore returns an empty store.
func NewStore() *Store {
	return &Store{
		operations: map[[2]string]*core.Operation{},
		auditAt:    map[string]time.Time{},
		jobs:       map[string]*core.Job{},
		sessions:   map[string]*core.AgentSession{},
		events:     map[string][]core.AgentEvent{},
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
	for _, a := range s.sessions {
		if a.WorkspaceID == id && a.UpdatedAt.After(last) {
			last = a.UpdatedAt
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
	for _, a := range s.sessions {
		if a.WorkspaceID == id && sessionActive(a.State) {
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

func (s *Store) CreateAgentSession(a *core.AgentSession) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.sessions[a.ID]; ok {
		return fmt.Errorf("agent session %s exists", a.ID)
	}
	now := s.now()
	s.sessions[a.ID] = &core.AgentSession{ID: a.ID, ClientID: a.ClientID, WorkspaceID: a.WorkspaceID, AgentID: a.AgentID,
		State: core.SessionStarting, CreatedAt: now, UpdatedAt: now}
	return nil
}

func sessionActive(state string) bool {
	return state == core.SessionStarting || state == core.SessionIdle || state == core.SessionRunning
}

func (s *Store) UpdateAgentSession(id string, u core.AgentSessionUpdate) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.sessions[id]
	if !ok || !sessionActive(a.State) {
		return nil
	}
	a.State = cmp.Or(u.State, a.State)
	a.ProtocolSessionID = cmp.Or(u.ProtocolSessionID, a.ProtocolSessionID)
	a.ErrorMessage = cmp.Or(u.ErrorMessage, a.ErrorMessage)
	if u.AddTurn {
		a.Turns++
	}
	a.UpdatedAt = s.now()
	return nil
}

func (s *Store) GetAgentSession(clientID, id string) (*core.AgentSession, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.sessions[id]
	if !ok || a.ClientID != clientID {
		return nil, core.ErrNotFound
	}
	c := *a
	return &c, nil
}

func (s *Store) AppendAgentEvent(sessionID string, turn int, typ, text string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	events := s.events[sessionID]
	s.events[sessionID] = append(events, core.AgentEvent{Seq: int64(len(events)) + 1, Turn: turn, Type: typ, Text: text, At: s.now()})
	return nil
}

func (s *Store) ListAgentEvents(sessionID string, after int64, limit int) ([]core.AgentEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []core.AgentEvent{}
	for _, e := range s.events[sessionID] {
		if e.Seq > after && len(out) < limit {
			out = append(out, e)
		}
	}
	return out, nil
}
