// Package job runs allowlisted commands in the background. Job state is kept
// in the store and stdout/stderr are kept in files, so callers can disconnect
// and read the output later by offset.
package job

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/takezoh/syntoria-reach/src/internal/command"
	"github.com/takezoh/syntoria-reach/src/internal/store"
)

// OutputLimit is the maximum size kept per output stream.
const OutputLimit = 64 << 20

// ErrNotRunning is returned when cancelling a job that has already ended.
var ErrNotRunning = errors.New("job is not running")

// Manager starts jobs and tracks the running ones.
type Manager struct {
	store   *store.Store
	dir     string
	mu      sync.Mutex
	running map[string]*run
}

type run struct {
	cmd       *exec.Cmd
	cancelled bool
	done      chan struct{}
}

// New returns a manager that keeps job output under dir.
func New(st *store.Store, dir string) *Manager {
	return &Manager{store: st, dir: dir, running: map[string]*run{}}
}

// Spec describes a job to start.
type Spec struct {
	ClientID    string
	WorkspaceID string
	Dir         string
	Executable  string
	Args        []string
	Timeout     time.Duration
}

// Start records and starts a job. The job keeps running after the caller
// returns; a start failure is recorded as a failed job.
func (m *Manager) Start(s Spec) (*store.Job, error) {
	id := "job_" + randomID()
	outDir := filepath.Join(m.dir, id)
	if err := os.MkdirAll(outDir, 0o700); err != nil {
		return nil, err
	}
	stdout, err := os.OpenFile(filepath.Join(outDir, "stdout"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	stderr, err := os.OpenFile(filepath.Join(outDir, "stderr"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		stdout.Close()
		return nil, err
	}
	if err := m.store.CreateJob(&store.Job{ID: id, ClientID: s.ClientID, WorkspaceID: s.WorkspaceID, Executable: s.Executable}); err != nil {
		stdout.Close()
		stderr.Close()
		return nil, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), s.Timeout)
	cmd := command.Command(ctx, s.Executable, s.Args, s.Dir, command.Env(nil))
	out, errOut := &command.LimitedWriter{W: stdout, N: OutputLimit}, &command.LimitedWriter{W: stderr, N: OutputLimit}
	cmd.Stdout, cmd.Stderr = out, errOut
	if err := cmd.Start(); err != nil {
		cancel()
		stdout.Close()
		stderr.Close()
		if ferr := m.store.FinishJob(id, store.JobFailed, nil, false, false, fmt.Sprintf("start: %v", err)); ferr != nil {
			return nil, ferr
		}
		return m.store.GetJob(s.ClientID, id)
	}
	r := &run{cmd: cmd, done: make(chan struct{})}
	m.mu.Lock()
	m.running[id] = r
	m.mu.Unlock()

	go func() {
		defer close(r.done)
		defer cancel()
		waitErr := cmd.Wait()
		command.KillGroup(cmd)
		stdout.Close()
		stderr.Close()
		m.mu.Lock()
		delete(m.running, id)
		cancelled := r.cancelled
		m.mu.Unlock()

		code := cmd.ProcessState.ExitCode()
		timedOut := errors.Is(ctx.Err(), context.DeadlineExceeded)
		state, msg := store.JobSucceeded, ""
		switch {
		case cancelled:
			state = store.JobCancelled
		case timedOut:
			state, msg = store.JobFailed, "timed out"
		case code != 0:
			state = store.JobFailed
		}
		var exitCode *int
		if code >= 0 {
			exitCode = &code
		}
		var exitErr *exec.ExitError
		if waitErr != nil && !errors.As(waitErr, &exitErr) && msg == "" {
			msg = waitErr.Error()
		}
		if err := m.store.FinishJob(id, state, exitCode, timedOut, out.Truncated || errOut.Truncated, msg); err != nil {
			// The job stays running in the store and becomes unknown on restart.
			fmt.Fprintf(os.Stderr, "record job %s: %v\n", id, err)
		}
	}()
	return m.store.GetJob(s.ClientID, id)
}

// Cancel kills a running job and waits until its outcome is recorded.
func (m *Manager) Cancel(id string) error {
	m.mu.Lock()
	r, ok := m.running[id]
	if ok {
		r.cancelled = true
	}
	m.mu.Unlock()
	if !ok {
		return ErrNotRunning
	}
	if err := command.KillGroup(r.cmd); err != nil {
		return err
	}
	<-r.done
	return nil
}

// Output is a chunk of a job's stdout or stderr.
type Output struct {
	Data   []byte `json:"data"`
	Offset int64  `json:"offset"`
	Size   int64  `json:"size"`
}

// ReadOutput reads up to limit bytes of a stream ("stdout" or "stderr").
func (m *Manager) ReadOutput(id, stream string, offset int64, limit int) (*Output, error) {
	if stream != "stdout" && stream != "stderr" {
		return nil, fmt.Errorf("unknown stream %q", stream)
	}
	f, err := os.Open(filepath.Join(m.dir, id, stream))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	buf := make([]byte, limit)
	n, err := f.ReadAt(buf, offset)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return &Output{Data: buf[:n], Offset: offset, Size: info.Size()}, nil
}

func randomID() string {
	b := make([]byte, 12)
	rand.Read(b)
	return hex.EncodeToString(b)
}
