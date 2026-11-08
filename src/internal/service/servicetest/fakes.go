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
	Agents   *Agents
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
		Agents:   &Agents{},
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
		Agents:     f.Agents,
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

// Agents runs fake sessions that the test drives with Emit, EndTurn and
// Exit. Like the real runtime it allows one turn at a time and reports
// nothing after Stop returned.
type Agents struct {
	// StartErr, if set, makes Start fail.
	StartErr error
	// OnStart, if set, runs at the start of Start, before the session runs.
	OnStart func()

	mu       sync.Mutex
	starts   map[string]core.AgentStart
	sessions map[string]*fakeSession
}

type fakeSession struct {
	events core.AgentEvents
	turn   int
	busy   bool
}

func (a *Agents) Start(_ context.Context, id string, s core.AgentStart, events core.AgentEvents) (string, error) {
	if a.OnStart != nil {
		a.OnStart()
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.StartErr != nil {
		return "", a.StartErr
	}
	if a.sessions == nil {
		a.starts, a.sessions = map[string]core.AgentStart{}, map[string]*fakeSession{}
	}
	a.starts[id] = s
	a.sessions[id] = &fakeSession{events: events}
	if s.Resume != "" {
		return s.Resume, nil
	}
	return "proto-" + id, nil
}

// Started returns how session id was started.
func (a *Agents) Started(id string) (core.AgentStart, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	s, ok := a.starts[id]
	return s, ok
}

// Active reports whether session id is running.
func (a *Agents) Active(id string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	_, ok := a.sessions[id]
	return ok
}

func (a *Agents) Prompt(id, text string) (int, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	s, ok := a.sessions[id]
	switch {
	case !ok:
		return 0, core.ErrSessionInactive
	case s.busy:
		return 0, core.ErrSessionBusy
	}
	if err := s.events.TurnStarted(id, s.turn+1); err != nil {
		return 0, err
	}
	s.turn++
	s.busy = true
	return s.turn, nil
}

// Emit reports an event of the turn in progress.
func (a *Agents) Emit(id, typ, text string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	s, ok := a.sessions[id]
	if !ok || !s.busy {
		return core.ErrSessionInactive
	}
	s.events.Event(id, s.turn, typ, text)
	return nil
}

// EndTurn completes the turn in progress with stopReason.
func (a *Agents) EndTurn(id, stopReason string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	s, ok := a.sessions[id]
	if !ok || !s.busy {
		return core.ErrSessionInactive
	}
	s.events.Event(id, s.turn, core.EventTurnCompleted, stopReason)
	s.events.TurnEnded(id, s.turn)
	s.busy = false
	return nil
}

// Exit makes the harness of session id exit on its own.
func (a *Agents) Exit(id string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	s, ok := a.sessions[id]
	if !ok {
		return core.ErrSessionInactive
	}
	delete(a.sessions, id)
	if s.busy {
		s.events.Event(id, s.turn, core.EventError, "harness stream ended")
	}
	s.events.Exited(id, s.turn)
	return nil
}

func (a *Agents) Stop(id string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	s, ok := a.sessions[id]
	if !ok {
		return core.ErrSessionInactive
	}
	delete(a.sessions, id)
	if s.busy {
		s.events.Event(id, s.turn, core.EventTurnCompleted, "cancelled")
	}
	return nil
}

// Orca keeps projects and their root entries in memory, as Orca reports
// them. Adding a folder named in FailAdd fails; with FailList set, listing
// fails as when Orca is down.
type Orca struct {
	FailAdd  map[string]bool // by folder name
	FailList bool
	// Now is the clock for last activity; it defaults to time.Now.
	Now func() time.Time

	mu    sync.Mutex
	repos []core.Repo
	roots map[string]*core.Worktree // repo ID -> root entry
	next  int
	calls []string
}

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

// Calls returns the Orca calls so far.
func (o *Orca) Calls() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return slices.Clone(o.calls)
}

// Folders keeps the set of existing folders in memory.
type Folders struct {
	mu   sync.Mutex
	dirs map[string]bool
}

func (f *Folders) Make(p string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.dirs[p] {
		return fmt.Errorf("mkdir %s: file exists", p)
	}
	if f.dirs == nil {
		f.dirs = map[string]bool{}
	}
	f.dirs[p] = true
	return nil
}

func (f *Folders) Remove(p string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.dirs, p)
	return nil
}

// Exists reports whether the folder p exists.
func (f *Folders) Exists(p string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.dirs[p]
}
