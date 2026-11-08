package servicetest

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/takezoh/syntoria-reach/src/internal/core"
)

// Records keeps the records of local workspaces (localws.Records) in
// memory, like the SQLite store.
type Records struct {
	// Now is the clock for last activity; it defaults to time.Now.
	Now func() time.Time
	// FailCreate makes CreateWorkspace fail.
	FailCreate bool

	mu    sync.Mutex
	items map[string]core.Workspace
}

// NewRecords returns an empty record keeper.
func NewRecords() *Records { return &Records{items: map[string]core.Workspace{}} }

func (r *Records) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Records) CreateWorkspace(w *core.Workspace) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.FailCreate {
		return fmt.Errorf("disk full")
	}
	for _, o := range r.items {
		if o.ID == w.ID || o.Path == w.Path {
			return fmt.Errorf("UNIQUE constraint failed")
		}
	}
	c := *w
	c.LastActivity = r.now().UTC()
	r.items[w.ID] = c
	return nil
}

func (r *Records) Workspaces() ([]*core.Workspace, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := []*core.Workspace{}
	for _, w := range r.items {
		out = append(out, &w)
	}
	slices.SortFunc(out, func(a, b *core.Workspace) int { return cmp.Or(cmp.Compare(a.Name, b.Name), cmp.Compare(a.ID, b.ID)) })
	return out, nil
}

func (r *Records) Workspace(id string) (*core.Workspace, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	w, ok := r.items[id]
	if !ok {
		return nil, core.ErrNotFound
	}
	return &w, nil
}

func (r *Records) UpdateWorkspace(id, note, status string) (*core.Workspace, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	w, ok := r.items[id]
	if !ok {
		return nil, core.ErrNotFound
	}
	if note != "" {
		w.Note = note
	}
	if status != "" {
		w.Status = status
	}
	w.LastActivity = r.now().UTC()
	r.items[id] = w
	return &w, nil
}

func (r *Records) DeleteWorkspace(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.items, id)
	return nil
}

// Agents runs fake agent processes (localws.Agents): every agent starts at
// once and replies to a prompt with "echo: <prompt>" as soon as it gets it.
type Agents struct {
	// FailStart makes Start fail as an agent that does not come up does.
	FailStart bool

	mu       sync.Mutex
	sessions map[string]*fakeAgent
	started  []core.Agent
}

type fakeAgent struct {
	dir    string
	agent  core.Agent
	state  string
	reply  string
	prompt int
}

func (a *Agents) Start(_ context.Context, id, dir string, ag core.Agent) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.started = append(a.started, ag)
	if a.FailStart {
		return core.Errorf(core.CodeHarnessFailed, "agent %q did not start", ag.ID)
	}
	if a.sessions == nil {
		a.sessions = map[string]*fakeAgent{}
	}
	a.sessions[id] = &fakeAgent{dir: dir, agent: ag, state: core.AgentIdle}
	return nil
}

func (a *Agents) Status(id string) (*core.AgentStatus, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	s, ok := a.sessions[id]
	if !ok {
		return nil, core.ErrNotFound
	}
	st := &core.AgentStatus{AgentSession: core.AgentSession{ID: id, State: s.state}}
	if s.state == core.AgentIdle {
		st.Reply = s.reply
	}
	return st, nil
}

func (a *Agents) Prompt(id, text string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	s, ok := a.sessions[id]
	switch {
	case !ok:
		return core.ErrNotFound
	case s.state != core.AgentIdle:
		return core.Errorf(core.CodeInvalidState, "the agent is %s", s.state)
	}
	s.prompt++
	s.reply = "echo: " + text
	return nil
}

func (a *Agents) Stop(id string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, ok := a.sessions[id]; !ok {
		return core.ErrNotFound
	}
	delete(a.sessions, id)
	return nil
}

// SetState makes the agent of session id idle, working or exited.
func (a *Agents) SetState(id, state string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sessions[id].state = state
}

// Prompts returns how many prompts the agent of session id got, and the
// folder it runs in.
func (a *Agents) Prompts(id string) (int, string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if s, ok := a.sessions[id]; ok {
		return s.prompt, s.dir
	}
	return 0, ""
}

// Started returns the agents started so far, in order.
func (a *Agents) Started() []core.Agent {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.started)
}

// Running returns how many agents run.
func (a *Agents) Running() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.sessions)
}
