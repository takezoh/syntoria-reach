package servicetest

import (
	"context"
	"fmt"
	"path"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/takezoh/syntoria-reach/src/internal/core"
	"github.com/takezoh/syntoria-reach/src/internal/service"
)

// Fakes holds one fake per service port.
type Fakes struct {
	Store    *Store
	Files    *Files
	Commands *Commands
	Jobs     *Jobs
	Orca     *Orca
	Folders  *Folders
}

// New returns fresh fakes.
func New() *Fakes {
	return &Fakes{
		Store:    NewStore(),
		Files:    &Files{},
		Commands: &Commands{},
		Jobs:     &Jobs{},
		Orca:     &Orca{},
		Folders:  &Folders{},
	}
}

// Deps wires the fakes as service dependencies. IDs are prefix + a counter.
func (f *Fakes) Deps() service.Deps {
	var mu sync.Mutex
	n := 0
	return service.Deps{
		Store:      f.Store,
		OpenFiles:  f.Files.Open,
		RunCommand: f.Commands.Run,
		Jobs:       f.Jobs,
		Orca:       f.Orca,
		Folders:    f.Folders,
		NewID: func(prefix string) string {
			mu.Lock()
			defer mu.Unlock()
			n++
			return fmt.Sprintf("%s%d", prefix, n)
		},
	}
}

// Files is an in-memory file system holding workspace roots by directory.
type Files struct {
	mu    sync.Mutex
	files map[string][]byte // dir + "/" + rel -> content
	open  map[string]int    // dir -> open roots
}

// Open opens the workspace rooted at dir; any directory exists.
func (f *Files) Open(dir string) (service.Files, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.open == nil {
		f.open = map[string]int{}
	}
	f.open[dir]++
	return &root{fs: f, dir: dir}, nil
}

// OpenRoots returns how many roots of dir are open.
func (f *Files) OpenRoots(dir string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.open[dir]
}

// Put stores a file.
func (f *Files) Put(dir, rel string, data []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.files == nil {
		f.files = map[string][]byte{}
	}
	f.files[path.Join(dir, rel)] = slices.Clone(data)
}

// Get returns a file's content.
func (f *Files) Get(dir, rel string) ([]byte, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.files[path.Join(dir, rel)]
	return slices.Clone(b), ok
}

type root struct {
	fs     *Files
	dir    string
	closed bool
}

func (r *root) List(rel string, limit int) ([]core.FileEntry, bool, error) {
	if rel == "" {
		rel = "."
	}
	if err := core.CheckPath(rel); err != nil {
		return nil, false, err
	}
	r.fs.mu.Lock()
	defer r.fs.mu.Unlock()
	base := path.Join(r.dir, rel)
	if _, ok := r.fs.files[base]; ok {
		return nil, false, core.ErrNotRegular // a file, not a directory
	}
	seen := map[string]core.FileEntry{}
	for name, data := range r.fs.files {
		rest, ok := strings.CutPrefix(name, base+"/")
		if !ok {
			continue
		}
		if first, _, nested := strings.Cut(rest, "/"); nested {
			seen[first] = core.FileEntry{Name: first, Type: "dir"}
		} else {
			seen[rest] = core.FileEntry{Name: rest, Type: "file", Size: int64(len(data))}
		}
	}
	if len(seen) == 0 && rel != "." {
		return nil, false, core.ErrNotFound
	}
	entries := []core.FileEntry{}
	for _, e := range seen {
		entries = append(entries, e)
	}
	slices.SortFunc(entries, func(a, b core.FileEntry) int { return strings.Compare(a.Name, b.Name) })
	if len(entries) > limit {
		return entries[:limit], true, nil
	}
	return entries, false, nil
}

func (r *root) Read(rel string, offset int64, limit int) (*core.FileChunk, error) {
	if err := core.CheckPath(rel); err != nil {
		return nil, err
	}
	r.fs.mu.Lock()
	defer r.fs.mu.Unlock()
	name := path.Join(r.dir, rel)
	data, ok := r.fs.files[name]
	if !ok {
		for other := range r.fs.files {
			if strings.HasPrefix(other, name+"/") {
				return nil, core.ErrNotRegular // a directory
			}
		}
		return nil, core.ErrNotFound
	}
	size := int64(len(data))
	start := min(offset, size)
	end := min(start+int64(limit), size)
	return &core.FileChunk{Data: slices.Clone(data[start:end]), Size: size, Offset: offset, EOF: end == size, Version: core.FileVersion(data)}, nil
}

func (r *root) Write(rel string, data []byte, expectedVersion string) (string, error) {
	if err := core.CheckPath(rel); err != nil {
		return "", err
	}
	r.fs.mu.Lock()
	defer r.fs.mu.Unlock()
	name := path.Join(r.dir, rel)
	current, exists := r.fs.files[name]
	if (exists && core.FileVersion(current) != expectedVersion) || (!exists && expectedVersion != "") {
		return "", core.ErrVersionConflict
	}
	if r.fs.files == nil {
		r.fs.files = map[string][]byte{}
	}
	r.fs.files[name] = slices.Clone(data)
	return core.FileVersion(data), nil
}

func (r *root) Close() error {
	r.fs.mu.Lock()
	defer r.fs.mu.Unlock()
	if !r.closed {
		r.closed = true
		r.fs.open[r.dir]--
	}
	return nil
}

// Commands records the commands it is asked to run.
type Commands struct {
	// Result decides the outcome of a command; by default it exits 0 with
	// the executable and arguments as stdout.
	Result func(core.CommandSpec) (*core.CommandResult, error)

	mu    sync.Mutex
	calls []core.CommandSpec
}

func (c *Commands) Run(_ context.Context, s core.CommandSpec) (*core.CommandResult, error) {
	c.mu.Lock()
	c.calls = append(c.calls, s)
	result := c.Result
	c.mu.Unlock()
	if result != nil {
		return result(s)
	}
	return &core.CommandResult{Stdout: []byte(strings.Join(append([]string{s.Executable}, s.Args...), " "))}, nil
}

// Calls returns the commands run so far.
func (c *Commands) Calls() []core.CommandSpec {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.calls)
}

// Jobs keeps started jobs running until the test ends them with Exit.
type Jobs struct {
	// StartErr, if set, makes Start fail.
	StartErr error

	mu      sync.Mutex
	specs   map[string]core.JobSpec
	running map[string]func(core.JobExit)
	output  map[string][]byte // id + "/" + stream
}

func (j *Jobs) Start(id string, s core.JobSpec, done func(core.JobExit)) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.StartErr != nil {
		return j.StartErr
	}
	if j.specs == nil {
		j.specs, j.running = map[string]core.JobSpec{}, map[string]func(core.JobExit){}
	}
	j.specs[id] = s
	j.running[id] = done
	return nil
}

// Spec returns the spec job id was started with.
func (j *Jobs) Spec(id string) (core.JobSpec, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	s, ok := j.specs[id]
	return s, ok
}

// Exit ends a running job with e and returns after its done callback.
func (j *Jobs) Exit(id string, e core.JobExit) error {
	j.mu.Lock()
	done, ok := j.running[id]
	delete(j.running, id)
	j.mu.Unlock()
	if !ok {
		return core.ErrJobNotRunning
	}
	done(e)
	return nil
}

func (j *Jobs) Cancel(id string) error {
	return j.Exit(id, core.JobExit{Cancelled: true})
}

// SetOutput replaces the content of a job's stream.
func (j *Jobs) SetOutput(id, stream string, data []byte) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.output == nil {
		j.output = map[string][]byte{}
	}
	j.output[id+"/"+stream] = slices.Clone(data)
}

func (j *Jobs) ReadOutput(id, stream string, offset int64, limit int) (*core.JobOutput, error) {
	if stream != "stdout" && stream != "stderr" {
		return nil, fmt.Errorf("unknown stream %q", stream)
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	data := j.output[id+"/"+stream]
	size := int64(len(data))
	start := min(offset, size)
	end := min(start+int64(limit), size)
	return &core.JobOutput{Data: slices.Clone(data[start:end]), Offset: offset, Size: size}, nil
}

// Orca keeps projects and their root entries in memory, as Orca reports
// them. Adding a folder named in FailAdd fails; with FailList set, listing
// fails as when Orca is down.
type Orca struct {
	FailAdd  map[string]bool // by folder name
	FailList bool
	// Now is the clock for last activity; it defaults to time.Now.
	Now func() time.Time

	// NoAgent makes new terminals run no agent, as when its command fails;
	// SilentAgent makes the agent not start a turn for a prompt, as one
	// stuck on a question would.
	NoAgent     bool
	SilentAgent bool

	mu        sync.Mutex
	repos     []core.Repo
	roots     map[string]*core.Worktree // repo ID -> root entry
	next      int
	calls     []string
	terminals map[string]*fakeTerminal // by handle
}

// fakeTerminal is a terminal with an agent that replies to every prompt
// with "echo: <prompt>".
type fakeTerminal struct {
	worktree string
	command  string
	open     bool
	agent    string // the agent running in it; empty for a shell
	waiting  string // what Orca saw the agent wait for from its user; empty when nothing
	blocked  string // what Orca saw the agent stopped at when waiting for it to be idle
	busy     bool   // in a turn
	turn     core.AgentTurn
}

// paneKey is the pane key Orca reports for terminal handle.
func paneKey(handle string) string { return "tab-" + handle + ":leaf-" + handle }

func (o *Orca) AddFolder(_ context.Context, p string) (string, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.calls = append(o.calls, "add "+p)
	if o.FailAdd[path.Base(p)] {
		return "", &core.OrcaError{Code: "add_failed", Message: "cannot add " + p}
	}
	o.next++
	id := fmt.Sprintf("repo-%d", o.next)
	o.add(core.Repo{ID: id, DisplayName: path.Base(p), Path: p, Kind: "folder"})
	return id, nil
}

// AddRepo registers a project as if someone added it in Orca, outside Reach.
func (o *Orca) AddRepo(r core.Repo) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.add(r)
}

func (o *Orca) add(r core.Repo) {
	if o.roots == nil {
		o.roots = map[string]*core.Worktree{}
	}
	now := time.Now
	if o.Now != nil {
		now = o.Now
	}
	o.repos = append(o.repos, r)
	o.roots[r.ID] = &core.Worktree{ID: r.ID + "::" + r.Path, RepoID: r.ID, Path: r.Path, WorkspaceStatus: "in-progress",
		IsMainWorktree: true, LastActivityAt: now().UnixMilli()}
}

func (o *Orca) Repos(context.Context) ([]core.Repo, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.calls = append(o.calls, "repos")
	if o.FailList {
		return nil, &core.OrcaError{Code: "runtime_unavailable", Message: "Orca is not running"}
	}
	return slices.Clone(o.repos), nil
}

func (o *Orca) Repo(_ context.Context, id string) (*core.Repo, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.calls = append(o.calls, "repo "+id)
	if o.FailList {
		return nil, &core.OrcaError{Code: "runtime_unavailable", Message: "Orca is not running"}
	}
	for _, r := range o.repos {
		if r.ID == id {
			return &r, nil
		}
	}
	return nil, core.ErrNotFound
}

func (o *Orca) Worktrees(_ context.Context, repoID string) ([]core.Worktree, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.calls = append(o.calls, "worktrees "+repoID)
	if o.FailList {
		return nil, &core.OrcaError{Code: "runtime_unavailable", Message: "Orca is not running"}
	}
	var out []core.Worktree
	for _, r := range o.repos {
		if repoID == "" || r.ID == repoID {
			out = append(out, *o.roots[r.ID])
		}
	}
	if repoID != "" && out == nil {
		return nil, core.ErrNotFound
	}
	return out, nil
}

func (o *Orca) SetWorktree(_ context.Context, id, comment, status string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.calls = append(o.calls, "set "+id+" "+comment+" "+status)
	var root *core.Worktree
	for _, w := range o.roots {
		if w.ID == id {
			root = w
		}
	}
	switch {
	case root == nil:
		return &core.OrcaError{Code: "selector_not_found", Message: id}
	case status != "" && !slices.Contains([]string{"todo", "in-progress", "in-review", "completed"}, status):
		return &core.OrcaError{Code: "invalid_workspace_status", Message: status}
	}
	if comment != "" {
		root.Comment = comment
	}
	if status != "" {
		root.WorkspaceStatus = status
	}
	return nil
}

func (o *Orca) DeleteProject(_ context.Context, repoID string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.calls = append(o.calls, "delete "+repoID)
	i := slices.IndexFunc(o.repos, func(r core.Repo) bool { return r.ID == repoID })
	if i < 0 {
		return &core.OrcaError{Code: "repo_not_found", Message: repoID}
	}
	o.repos = slices.Delete(o.repos, i, i+1)
	delete(o.roots, repoID)
	return nil
}

// Root returns a copy of a project's root entry, or nil.
func (o *Orca) Root(repoID string) *core.Worktree {
	o.mu.Lock()
	defer o.mu.Unlock()
	if w, ok := o.roots[repoID]; ok {
		c := *w
		return &c
	}
	return nil
}

// Touch sets a project's last activity in Orca.
func (o *Orca) Touch(repoID string, at time.Time) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.roots[repoID].LastActivityAt = at.UnixMilli()
}

func (o *Orca) CreateTerminal(_ context.Context, worktreeID, title, command string) (string, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.calls = append(o.calls, "terminal "+worktreeID+" "+command)
	o.next++
	h := fmt.Sprintf("term-%d", o.next)
	if o.terminals == nil {
		o.terminals = map[string]*fakeTerminal{}
	}
	t := &fakeTerminal{worktree: worktreeID, command: command, open: true, agent: "agent"}
	if o.NoAgent {
		t.agent = ""
	}
	o.terminals[h] = t
	return h, nil
}

func (o *Orca) Terminals(context.Context) ([]core.Terminal, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.FailList {
		return nil, &core.OrcaError{Code: "runtime_unavailable", Message: "Orca is not running"}
	}
	var out []core.Terminal
	for h, t := range o.terminals {
		if t.open {
			out = append(out, core.Terminal{Handle: h, WorktreeID: t.worktree, Connected: true, AgentIdentity: t.agent})
		}
	}
	return out, nil
}

// terminal returns open terminal handle, or nil. o.mu is held.
func (o *Orca) terminal(handle string) *fakeTerminal {
	if t := o.terminals[handle]; t != nil && t.open {
		return t
	}
	return nil
}

func (o *Orca) ShowTerminal(_ context.Context, handle string) (*core.TerminalStatus, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	t := o.terminal(handle)
	if t == nil {
		return nil, core.ErrNotFound
	}
	tab, leaf, _ := strings.Cut(paneKey(handle), ":")
	return &core.TerminalStatus{Terminal: core.Terminal{Handle: handle, WorktreeID: t.worktree, Connected: true, AgentIdentity: t.agent,
		TabID: tab, LeafID: leaf}, WaitingOnHuman: t.waiting}, nil
}

// WaitIdle reports the agent idle unless it is in a turn, waits for its
// user, or is gone; a shell is never idle as a TUI is.
func (o *Orca) WaitIdle(_ context.Context, handle string, _ time.Duration) (bool, string, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	t := o.terminal(handle)
	if t == nil {
		return false, "", core.ErrNotFound
	}
	if t.blocked != "" {
		return false, t.blocked, nil
	}
	return t.agent != "" && !t.busy && t.waiting == "", "", nil
}

// AgentTurn reports the agent's turn as its status hooks would: running
// during a turn, with the agent's reply once it ended.
func (o *Orca) AgentTurn(_ context.Context, key string) (*core.AgentTurn, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	for h, t := range o.terminals {
		if t.open && t.agent != "" && paneKey(h) == key {
			turn := t.turn
			turn.Running = turn.Running || t.busy
			return &turn, nil
		}
	}
	return nil, nil
}

// Hooks sets the agent's turn in terminal handle as its status hooks report
// it, which may lag behind what its terminal shows.
func (o *Orca) Hooks(handle string, turn core.AgentTurn) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.terminals[handle].turn = turn
}

// AskUser makes the agent in terminal handle wait for its user, as Orca
// shows it: reason is the evidence Orca reports, core.WaitUnknown when Orca
// cannot tell.
func (o *Orca) AskUser(handle, reason string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.terminals[handle].waiting = reason
}

// Block makes Orca see the agent in terminal handle stopped at reason when
// it waits for the agent to be idle.
func (o *Orca) Block(handle, reason string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.terminals[handle].blocked = reason
}

// Busy puts the agent in terminal handle in a turn, or out of it.
func (o *Orca) Busy(handle string, busy bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.terminals[handle].busy = busy
}

// ExitAgent ends the agent in terminal handle and leaves its shell, as when
// the agent quits.
func (o *Orca) ExitAgent(handle string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.terminals[handle].agent = ""
}

func (o *Orca) SendPrompt(_ context.Context, handle, text string) ([]string, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.calls = append(o.calls, "send "+handle+" "+text)
	t := o.terminal(handle)
	if t == nil {
		return nil, &core.OrcaError{Code: "terminal_handle_stale", Message: handle}
	}
	if o.SilentAgent {
		return []string{"input_accepted"}, nil
	}
	t.turn = core.AgentTurn{Reply: "echo: " + text}
	return []string{"input_accepted", "turn_started"}, nil
}

func (o *Orca) CloseTerminal(_ context.Context, handle string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.calls = append(o.calls, "close "+handle)
	if t, ok := o.terminals[handle]; ok {
		t.open = false
	}
	return nil
}

// TerminalCommand returns what was typed into terminal handle and whether
// it is open.
func (o *Orca) TerminalCommand(handle string) (string, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	t, ok := o.terminals[handle]
	if !ok {
		return "", false
	}
	return t.command, t.open
}

// Calls returns the Orca calls so far.
func (o *Orca) Calls() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return slices.Clone(o.calls)
}

// Folders keeps the set of existing folders in memory.
type Folders struct {
	mu   sync.Mutex
	dirs map[string]time.Time // folder -> when something in it last changed
}

func (f *Folders) Make(p string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.dirs[p]; ok {
		return fmt.Errorf("mkdir %s: file exists", p)
	}
	if f.dirs == nil {
		f.dirs = map[string]time.Time{}
	}
	f.dirs[p] = time.Now()
	return nil
}

func (f *Folders) Remove(p string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.dirs, p)
	return nil
}

func (f *Folders) List(dir string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var names []string
	for p := range f.dirs {
		if path.Dir(p) == dir {
			names = append(names, path.Base(p))
		}
	}
	return names, nil
}

func (f *Folders) ChangedSince(p string, t time.Time) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	at, ok := f.dirs[p]
	if !ok {
		return false, fmt.Errorf("lstat %s: no such file or directory", p)
	}
	return !at.Before(t), nil
}

// Change sets when something in folder p last changed.
func (f *Folders) Change(p string, at time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dirs[p] = at
}

// Exists reports whether the folder p exists.
func (f *Folders) Exists(p string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.dirs[p]
	return ok
}
