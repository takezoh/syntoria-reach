// Package orcaws is the Orca workspace backend. A workspace is a folder
// project of the Orca server named with core.WorkspacePrefix, whose root
// entry carries the note (comment) and status (workspace status). An agent
// session is an Orca terminal of a workspace running the agent's command:
// prompts are typed into it and replies are read from Orca's agent status.
// Reach keeps nothing about either and reads them from Orca on every
// request. Every terminal in a Reach workspace is a session, and no other
// terminal is.
package orcaws

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"
	"time"

	"github.com/takezoh/syntoria-reach/src/internal/core"
)

const (
	// startWait is how long starting a session waits for the agent to wait
	// for its first prompt; the remote gateway gives a request 30 s.
	startWait = 15 * time.Second
	// idleWait is how long Reach looks for the agent waiting for a prompt
	// before it reports the agent working.
	idleWait = 3 * time.Second
)

// Orca owns the projects that are workspaces and the terminals of their
// agents. Failures Orca reports are *core.OrcaError.
type Orca interface {
	// AddFolder registers an existing folder as a folder project and
	// returns its repo ID.
	AddFolder(ctx context.Context, path string) (string, error)
	Repos(ctx context.Context) ([]core.Repo, error)
	// Repo returns one project; core.ErrNotFound if Orca has none.
	Repo(ctx context.Context, id string) (*core.Repo, error)
	// Worktrees lists the workspace entries of one project, or of every
	// project when repoID is empty; core.ErrNotFound for an unknown
	// project. A partial list is an error.
	Worktrees(ctx context.Context, repoID string) ([]core.Worktree, error)
	// SetWorktree changes a workspace entry's comment and workspace
	// status; an empty value leaves that one unchanged.
	SetWorktree(ctx context.Context, id, comment, status string) error
	// DeleteProject removes a project, with its saved details, from Orca;
	// its folder stays.
	DeleteProject(ctx context.Context, repoID string) error

	// CreateTerminal opens a terminal in workspace entry worktreeID, types
	// command into its shell, and returns its handle.
	CreateTerminal(ctx context.Context, worktreeID, title, command string) (string, error)
	Terminals(ctx context.Context) ([]core.Terminal, error)
	// ShowTerminal shows one terminal; core.ErrNotFound if Orca has no
	// terminal with that handle, as once it closed.
	ShowTerminal(ctx context.Context, handle string) (*core.TerminalStatus, error)
	// WaitIdle waits up to timeout for the agent in terminal handle to wait
	// for a prompt, and reports whether it does. Orca may instead see it
	// stopped at something only a person can answer: blocked says what.
	WaitIdle(ctx context.Context, handle string, timeout time.Duration) (idle bool, blocked string, err error)
	// AgentTurn reads the latest turn of the agent in the terminal whose
	// pane key is paneKey, as Orca's agent status hooks report it; nil when
	// Orca reports no agent there.
	AgentTurn(ctx context.Context, paneKey string) (*core.AgentTurn, error)
	// SendPrompt types and submits text to the agent in terminal handle and
	// returns the stages Orca observed (input_accepted, turn_started).
	SendPrompt(ctx context.Context, handle, text string) ([]string, error)
	CloseTerminal(ctx context.Context, handle string) error
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

// Workspaces keeps workspaces on the Orca server, with their folders under
// dir.
type Workspaces struct {
	orca    Orca
	folders Folders
	dir     string
}

// New returns the backend for the Orca server reached through o, creating
// workspace folders under dir.
func New(o Orca, f Folders, dir string) *Workspaces {
	return &Workspaces{orca: o, folders: f, dir: dir}
}

// Create creates the folder dir/name and registers it in Orca as a folder
// project with the note.
func (b *Workspaces) Create(ctx context.Context, name, note string) (*core.Workspace, error) {
	path := filepath.Join(b.dir, name)
	if err := b.folders.Make(path); err != nil {
		return nil, core.Errorf(core.CodeInvalidState, "cannot create the workspace folder: %v", err)
	}
	id, err := b.orca.AddFolder(ctx, path)
	if err != nil {
		// The folder is new and empty; nothing else refers to it.
		if rerr := b.folders.Remove(path); rerr != nil {
			slog.Warn("cannot remove the folder of a workspace Orca refused", "path", path, "err", rerr)
		}
		return nil, orcaError(err)
	}
	r, root, err := b.workspace(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := b.orca.SetWorktree(ctx, root.ID, note, ""); err != nil {
		return nil, orcaError(err)
	}
	root.Comment = note
	return core.NewWorkspace(r, root), nil
}

// List returns the projects that are workspaces. An Orca that cannot be
// reached is an error, never an empty list.
func (b *Workspaces) List(ctx context.Context) ([]*core.Workspace, error) {
	repos, roots, err := b.projects(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*core.Workspace, 0, len(repos))
	for i := range repos {
		out = append(out, core.NewWorkspace(&repos[i], roots[repos[i].ID]))
	}
	return out, nil
}

// Get returns workspace id.
func (b *Workspaces) Get(ctx context.Context, id string) (*core.Workspace, error) {
	r, root, err := b.workspace(ctx, id)
	if err != nil {
		return nil, err
	}
	return core.NewWorkspace(r, root), nil
}

// Update changes the comment and workspace status of the workspace's root
// entry. Orca decides which statuses exist.
func (b *Workspaces) Update(ctx context.Context, id, note, status string) (*core.Workspace, error) {
	r, root, err := b.workspace(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := b.orca.SetWorktree(ctx, root.ID, note, status); err != nil {
		return nil, orcaError(err)
	}
	if note != "" {
		root.Comment = note
	}
	if status != "" {
		root.WorkspaceStatus = status
	}
	return core.NewWorkspace(r, root), nil
}

// Remove removes the workspace's project from Orca (with --force, which
// also detaches terminals open in Orca), and its folder if Reach created
// it. A folder that cannot be removed now is swept later: no project has it
// any more.
func (b *Workspaces) Remove(ctx context.Context, w *core.Workspace) error {
	if err := b.orca.DeleteProject(ctx, w.ID); err != nil {
		return orcaError(err)
	}
	if core.OwnFolder(b.dir, w.Path, w.Name) {
		if err := b.folders.Remove(w.Path); err != nil {
			slog.Warn("cannot remove the folder of a removed workspace", "path", w.Path, "err", err)
		}
	}
	return nil
}

// Sweep removes the folders Reach created whose Orca projects are gone, as
// when a project is removed in Orca, which leaves its folder. A folder goes
// once nothing in it changed since since, so that a folder being created,
// or one Orca leaves out for a while, stays; a folder any Orca project has,
// under any name, stays.
func (b *Workspaces) Sweep(ctx context.Context, since time.Time) ([]string, error) {
	names, err := b.folders.List(b.dir)
	if err != nil {
		return nil, fmt.Errorf("list the workspace folders: %w", err)
	}
	if len(names) == 0 {
		return nil, nil
	}
	repos, err := b.orca.Repos(ctx)
	if err != nil {
		return nil, orcaError(err)
	}
	inOrca := map[string]bool{}
	for _, r := range repos {
		inOrca[filepath.Clean(r.Path)] = true
	}
	return sweep(b.folders, b.dir, names, inOrca, since)
}

// sweep removes the folders Reach created under dir (of names) that no
// workspace has (inUse, by path) and in which nothing changed since since.
func sweep(f Folders, dir string, names []string, inUse map[string]bool, since time.Time) ([]string, error) {
	var removed []string
	var errs []error
	for _, name := range names {
		path := filepath.Join(dir, name)
		if !core.OwnFolder(dir, path, name) || inUse[path] {
			continue
		}
		changed, err := f.ChangedSince(path, since)
		if err == nil && !changed {
			if err = f.Remove(path); err == nil {
				removed = append(removed, name)
			}
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("folder %s: %w", path, err))
		}
	}
	return removed, errors.Join(errs...)
}

// Busy returns the workspaces with an open terminal in Orca.
func (b *Workspaces) Busy(ctx context.Context) (map[string]bool, error) {
	terms, err := b.orca.Terminals(ctx)
	if err != nil {
		return nil, orcaError(err)
	}
	open := map[string]bool{}
	for _, t := range terms {
		if t.Connected {
			open[core.WorkspaceOfEntry(t.WorktreeID)] = true
		}
	}
	return open, nil
}

// StartAgent types the agent's command (shell-quoted) into a new terminal of
// the workspace's root entry, and returns once Orca sees the agent idle at
// its prompt, or after startWait. If no agent runs in the terminal by then,
// it closes the terminal.
func (b *Workspaces) StartAgent(ctx context.Context, w *core.Workspace, a core.Agent) (*core.AgentSession, error) {
	_, root, err := b.workspace(ctx, w.ID)
	if err != nil {
		return nil, err
	}
	handle, err := b.orca.CreateTerminal(ctx, root.ID, "reach "+a.ID, core.ShellCommand(a.Command))
	if err != nil {
		return nil, orcaError(err)
	}
	// Until the agent is up, the terminal runs only the shell.
	_, _, err = b.orca.WaitIdle(ctx, handle, startWait)
	var st *core.AgentStatus
	switch {
	case errors.Is(err, core.ErrNotFound):
		err = core.Errorf(core.CodeHarnessFailed, "the terminal of agent %q closed as the agent started", a.ID)
	case err != nil:
		err = orcaError(err)
	default:
		st, err = b.Session(ctx, handle)
		switch {
		case errors.Is(err, core.ErrNotFound):
			err = core.Errorf(core.CodeHarnessFailed, "the terminal of agent %q closed as the agent started", a.ID)
		case err == nil && st.State == core.AgentExited:
			err = core.Errorf(core.CodeHarnessFailed, "agent %q did not start in its terminal; check its command", a.ID)
		}
	}
	if err != nil {
		if cerr := b.orca.CloseTerminal(ctx, handle); cerr != nil {
			slog.Warn("cannot close the terminal of an agent that failed to start", "terminal", handle, "err", cerr)
		}
		return nil, err
	}
	return &st.AgentSession, nil
}

// Session reads the terminal of session id from Orca, with what its agent
// does and, once it waits for a prompt, its reply.
func (b *Workspaces) Session(ctx context.Context, id string) (*core.AgentStatus, error) {
	t, err := b.terminal(ctx, id)
	if err != nil {
		return nil, err
	}
	state, turn, err := b.agentState(ctx, t)
	if err != nil {
		return nil, err
	}
	st := &core.AgentStatus{AgentSession: core.AgentSession{
		ID: id, Workspace: core.WorkspaceOfEntry(t.WorktreeID), Agent: t.AgentIdentity, State: state, WaitingOnHuman: t.WaitingOnHuman,
	}}
	if state == core.AgentIdle && turn != nil {
		st.Reply, st.ReplyTruncated = turn.Reply, turn.Truncated
	}
	return st, nil
}

// Prompt types text into the terminal of session id and submits it
// (orca terminal send --enter). The prompt is delivered once Orca observed
// the agent start its turn. id is a session Session returned.
func (b *Workspaces) Prompt(ctx context.Context, id, text string) ([]string, error) {
	stages, err := b.orca.SendPrompt(ctx, id, text)
	if err != nil {
		return nil, orcaError(err)
	}
	if !slices.Contains(stages, "turn_started") {
		return nil, core.Errorf(core.CodeHarnessFailed, "the agent did not start a turn (Orca observed %v); it may be waiting for input in Orca", stages)
	}
	return stages, nil
}

// Stop closes the terminal of session id, which ends the agent. Orca no
// longer has its handle afterwards, so the session is gone.
func (b *Workspaces) Stop(ctx context.Context, id string) error {
	if _, err := b.terminal(ctx, id); err != nil {
		return err
	}
	if err := b.orca.CloseTerminal(ctx, id); err != nil {
		return orcaError(err)
	}
	return nil
}

// agentState tells what the agent in terminal t does and, when it waits for
// a prompt, returns its latest turn as Orca's agent status reports it (nil
// when Orca reports none). Orca tells a question to the user from a turn
// only through what it saw of the wait, and an agent waiting for a prompt
// only by seeing it idle within idleWait; until the agent's status hooks
// report the turn ended, its reply is not there yet, so the agent is still
// working.
func (b *Workspaces) agentState(ctx context.Context, t *core.TerminalStatus) (string, *core.AgentTurn, error) {
	switch {
	case t.AgentIdentity == "":
		return core.AgentExited, nil, nil
	case t.WaitingOnHuman != "":
		return core.AgentWaitingOnHuman, nil, nil
	}
	idle, blocked, err := b.orca.WaitIdle(ctx, t.Handle, idleWait)
	switch {
	case errors.Is(err, core.ErrNotFound):
		return "", nil, core.ErrNotFound
	case err != nil:
		return "", nil, orcaError(err)
	case blocked != "":
		t.WaitingOnHuman = blocked
		return core.AgentWaitingOnHuman, nil, nil
	case !idle:
		return core.AgentWorking, nil, nil
	}
	var turn *core.AgentTurn
	if key := t.PaneKey(); key != "" {
		if turn, err = b.orca.AgentTurn(ctx, key); err != nil {
			return "", nil, orcaError(err)
		}
	}
	if turn != nil && turn.Running {
		return core.AgentWorking, nil, nil
	}
	return core.AgentIdle, turn, nil
}

// terminal reads the terminal of session id from Orca. It must be open in a
// Reach workspace; otherwise the session is core.ErrNotFound.
func (b *Workspaces) terminal(ctx context.Context, id string) (*core.TerminalStatus, error) {
	t, err := b.orca.ShowTerminal(ctx, id)
	if errors.Is(err, core.ErrNotFound) {
		return nil, core.ErrNotFound
	}
	if err != nil {
		return nil, orcaError(err)
	}
	if !t.Connected || t.Handle != id {
		return nil, core.ErrNotFound
	}
	if _, err := b.repo(ctx, core.WorkspaceOfEntry(t.WorktreeID)); err != nil {
		return nil, core.ErrNotFound
	}
	return t, nil
}

// projects reads the projects that are workspaces and their root entries,
// by repo ID, from Orca.
func (b *Workspaces) projects(ctx context.Context) ([]core.Repo, map[string]*core.Worktree, error) {
	repos, err := b.orca.Repos(ctx)
	if err != nil {
		return nil, nil, orcaError(err)
	}
	repos = slices.DeleteFunc(repos, func(r core.Repo) bool { return !core.IsWorkspace(&r) })
	if len(repos) == 0 {
		return repos, nil, nil
	}
	wts, err := b.orca.Worktrees(ctx, "")
	if err != nil {
		return nil, nil, orcaError(err)
	}
	roots := map[string]*core.Worktree{}
	for i := range wts {
		if wts[i].IsMainWorktree {
			roots[wts[i].RepoID] = &wts[i]
		}
	}
	return repos, roots, nil
}

// repo reads the project of workspace id from Orca.
func (b *Workspaces) repo(ctx context.Context, id string) (*core.Repo, error) {
	r, err := b.orca.Repo(ctx, id)
	if errors.Is(err, core.ErrNotFound) || (err == nil && !core.IsWorkspace(r)) {
		return nil, core.ErrNotFound
	}
	if err != nil {
		return nil, orcaError(err)
	}
	return r, nil
}

// workspace reads the project of workspace id and its root entry from Orca.
func (b *Workspaces) workspace(ctx context.Context, id string) (*core.Repo, *core.Worktree, error) {
	r, err := b.repo(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	wts, err := b.orca.Worktrees(ctx, id)
	if errors.Is(err, core.ErrNotFound) {
		return nil, nil, core.ErrNotFound
	}
	if err != nil {
		return nil, nil, orcaError(err)
	}
	for i := range wts {
		if wts[i].IsMainWorktree {
			return r, &wts[i], nil
		}
	}
	return nil, nil, core.Errorf(core.CodeOrcaFailed, "Orca reported no root entry for workspace %q", id)
}

// orcaError classifies a failed Orca call; every failure is orca_failed.
func orcaError(err error) error {
	if e := core.Classify(err); e != nil && e.Code == core.CodeOrcaFailed {
		return e
	}
	return core.Errorf(core.CodeOrcaFailed, "%v", err)
}
