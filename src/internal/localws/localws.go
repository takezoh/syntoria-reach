// Package localws is the workspace backend for nodes without Orca. A
// workspace is a folder named with core.WorkspacePrefix under the
// workspaces folder, which Reach records with its note and status. An agent
// session is an agent Reach runs as a process of the daemon in the
// workspace folder (package agent); sessions are kept in memory, since the
// agents are children of the daemon and end with it.
package localws

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/takezoh/syntoria-reach/src/internal/core"
)

// Records keeps the records of local workspaces.
type Records interface {
	// CreateWorkspace records w; its last activity is now.
	CreateWorkspace(w *core.Workspace) error
	Workspaces() ([]*core.Workspace, error)
	// Workspace returns one record; core.ErrNotFound if there is none.
	Workspace(id string) (*core.Workspace, error)
	// UpdateWorkspace changes a record's note and status (an empty value
	// leaves that one unchanged) and makes now its last activity;
	// core.ErrNotFound if there is no such record.
	UpdateWorkspace(id, note, status string) (*core.Workspace, error)
	DeleteWorkspace(id string) error
}

// Folders creates and removes workspace folders.
type Folders interface {
	// Make creates the folder path, which must not exist yet, and its
	// parents.
	Make(path string) error
	// Remove removes the folder path and everything in it.
	Remove(path string) error
	// List returns the names of the folders in dir, none when dir does not
	// exist.
	List(dir string) ([]string, error)
	// ChangedSince reports whether the folder path, or anything in it,
	// changed at or after t.
	ChangedSince(path string, t time.Time) (bool, error)
}

// Agents runs agents as processes, one per session (see agent.Runtime).
type Agents interface {
	Start(ctx context.Context, id, dir string, a core.Agent) error
	// Status reports the agent's state and reply; core.ErrNotFound for an
	// unknown session.
	Status(id string) (*core.AgentStatus, error)
	Prompt(id, text string) error
	Stop(id string) error
}

// Workspaces keeps local workspaces under dir.
type Workspaces struct {
	dir     string
	records Records
	folders Folders
	agents  Agents
	newID   func(prefix string) string

	mu       sync.Mutex
	sessions map[string]session // by session ID
}

type session struct {
	workspace string
	agent     string
}

// New returns the backend keeping workspace folders under dir.
func New(dir string, r Records, f Folders, a Agents, newID func(prefix string) string) *Workspaces {
	return &Workspaces{dir: dir, records: r, folders: f, agents: a, newID: newID, sessions: map[string]session{}}
}

// Create creates the folder dir/name and records the workspace, in
// progress, with the note.
func (b *Workspaces) Create(ctx context.Context, name, note string) (*core.Workspace, error) {
	path := filepath.Join(b.dir, name)
	if err := b.folders.Make(path); err != nil {
		return nil, core.Errorf(core.CodeInvalidState, "cannot create the workspace folder: %v", err)
	}
	w := &core.Workspace{ID: b.newID("ws_"), Name: name, Note: note, Status: core.WorkspaceInProgress, Path: path}
	if err := b.records.CreateWorkspace(w); err != nil {
		// The folder is new and empty; nothing else refers to it.
		if rerr := b.folders.Remove(path); rerr != nil {
			slog.Warn("cannot remove the folder of a workspace that was not recorded", "path", path, "err", rerr)
		}
		return nil, err
	}
	return b.records.Workspace(w.ID)
}

func (b *Workspaces) List(ctx context.Context) ([]*core.Workspace, error) {
	return b.records.Workspaces()
}

func (b *Workspaces) Get(ctx context.Context, id string) (*core.Workspace, error) {
	return b.records.Workspace(id)
}

// Update changes the note and status; a local workspace knows only the
// statuses in core.WorkspaceStatuses.
func (b *Workspaces) Update(ctx context.Context, id, note, status string) (*core.Workspace, error) {
	if status != "" && !slices.Contains(core.WorkspaceStatuses, status) {
		return nil, core.Errorf(core.CodeInvalidRequest, "status must be one of %v", core.WorkspaceStatuses)
	}
	return b.records.UpdateWorkspace(id, note, status)
}

// Remove forgets the workspace and removes its folder if Reach created it.
// A folder that cannot be removed now is swept later: no record has it any
// more.
func (b *Workspaces) Remove(ctx context.Context, w *core.Workspace) error {
	if err := b.records.DeleteWorkspace(w.ID); err != nil {
		return err
	}
	if core.OwnFolder(b.dir, w.Path, w.Name) {
		if err := b.folders.Remove(w.Path); err != nil {
			slog.Warn("cannot remove the folder of a removed workspace", "path", w.Path, "err", err)
		}
	}
	return nil
}

// Sweep removes the folders Reach created that no workspace record has, as
// one left by a daemon that stopped while creating a workspace, once
// nothing in them changed since since.
func (b *Workspaces) Sweep(ctx context.Context, since time.Time) ([]string, error) {
	names, err := b.folders.List(b.dir)
	if err != nil {
		return nil, fmt.Errorf("list the workspace folders: %w", err)
	}
	if len(names) == 0 {
		return nil, nil
	}
	list, err := b.records.Workspaces()
	if err != nil {
		return nil, err
	}
	recorded := map[string]bool{}
	for _, w := range list {
		recorded[filepath.Clean(w.Path)] = true
	}
	var removed []string
	var errs []error
	for _, name := range names {
		path := filepath.Join(b.dir, name)
		if !core.OwnFolder(b.dir, path, name) || recorded[path] {
			continue
		}
		changed, err := b.folders.ChangedSince(path, since)
		if err == nil && !changed {
			if err = b.folders.Remove(path); err == nil {
				removed = append(removed, name)
			}
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("folder %s: %w", path, err))
		}
	}
	return removed, errors.Join(errs...)
}

// Busy returns the workspaces with a session that was not stopped.
func (b *Workspaces) Busy(ctx context.Context) (map[string]bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	busy := map[string]bool{}
	for _, s := range b.sessions {
		busy[s.workspace] = true
	}
	return busy, nil
}

// StartAgent runs agent a in the workspace folder as a new session.
func (b *Workspaces) StartAgent(ctx context.Context, w *core.Workspace, a core.Agent) (*core.AgentSession, error) {
	id := b.newID("ses_")
	b.mu.Lock()
	b.sessions[id] = session{workspace: w.ID, agent: a.ID}
	b.mu.Unlock()
	if err := b.agents.Start(ctx, id, w.Path, a); err != nil {
		b.forget(id)
		return nil, err
	}
	st, err := b.Session(ctx, id)
	if err != nil {
		return nil, err
	}
	return &st.AgentSession, nil
}

// Session reads session id from the runtime running its agent.
func (b *Workspaces) Session(ctx context.Context, id string) (*core.AgentStatus, error) {
	s, ok := b.session(id)
	if !ok {
		return nil, core.ErrNotFound
	}
	st, err := b.agents.Status(id)
	if err != nil {
		return nil, err
	}
	st.ID, st.Workspace, st.Agent = id, s.workspace, s.agent
	return st, nil
}

// Prompt starts the agent's turn with text.
func (b *Workspaces) Prompt(ctx context.Context, id, text string) ([]string, error) {
	if _, ok := b.session(id); !ok {
		return nil, core.ErrNotFound
	}
	if err := b.agents.Prompt(id, text); err != nil {
		return nil, err
	}
	return []string{"turn_started"}, nil
}

// Stop ends the session's agent and forgets the session.
func (b *Workspaces) Stop(ctx context.Context, id string) error {
	if _, ok := b.session(id); !ok {
		return core.ErrNotFound
	}
	err := b.agents.Stop(id)
	if err != nil && !errors.Is(err, core.ErrNotFound) {
		return err
	}
	b.forget(id)
	return nil
}

func (b *Workspaces) session(id string) (session, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	s, ok := b.sessions[id]
	return s, ok
}

func (b *Workspaces) forget(id string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.sessions, id)
}
