package servicetest

import (
	"context"
	"fmt"
	"path"
	"slices"
	"strings"
	"sync"

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
}

// New returns fresh fakes.
func New() *Fakes {
	return &Fakes{
		Store:    NewStore(),
		Files:    &Files{},
		Commands: &Commands{},
		Jobs:     &Jobs{},
		Agents:   &Agents{},
		Orca:     &Orca{Root: "/orca"},
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

// Orca keeps worktrees in memory under Root. A worktree in FailCreate
// cannot be created and one in FailRemove cannot be removed.
type Orca struct {
	Root       string
	FailCreate map[string]bool // by name
	FailRemove map[string]bool // by name

	mu        sync.Mutex
	worktrees map[string]*core.Worktree // by ID
	calls     []string
}

func (o *Orca) CreateWorktree(_ context.Context, repoPath, name string) (*core.Worktree, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.calls = append(o.calls, "create "+repoPath+" "+name)
	if o.FailCreate[name] {
		return nil, &core.OrcaError{Code: "git_failed", Message: "cannot create worktree"}
	}
	id := repoPath + "::" + name
	if _, ok := o.worktrees[id]; ok {
		return nil, &core.OrcaError{Code: "worktree_exists", Message: name}
	}
	if o.worktrees == nil {
		o.worktrees = map[string]*core.Worktree{}
	}
	wt := &core.Worktree{ID: id, Path: path.Join(o.Root, name), Branch: "refs/heads/" + name}
	o.worktrees[id] = wt
	c := *wt
	return &c, nil
}

func (o *Orca) RemoveWorktree(_ context.Context, id string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.calls = append(o.calls, "remove "+id)
	wt, ok := o.worktrees[id]
	switch {
	case !ok:
		return &core.OrcaError{Code: "worktree_not_found", Message: id}
	case o.FailRemove[path.Base(wt.Path)]:
		return &core.OrcaError{Code: "worktree_dirty", Message: "worktree has uncommitted changes"}
	}
	delete(o.worktrees, id)
	return nil
}

// Calls returns the Orca calls so far.
func (o *Orca) Calls() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return slices.Clone(o.calls)
}

// Worktrees returns the IDs of the existing worktrees.
func (o *Orca) Worktrees() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	var ids []string
	for id := range o.worktrees {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}
