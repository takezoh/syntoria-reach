// Package job runs commands in the background and keeps their stdout/stderr
// in files, so callers can disconnect and read the output later by offset.
// It records nothing itself: the caller learns what happened through the
// done callback.
package job

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"

	"github.com/takezoh/syntoria-reach/src/internal/command"
	"github.com/takezoh/syntoria-reach/src/internal/core"
)

// OutputLimit is the maximum size kept per output stream.
const OutputLimit = 64 << 20

// Runner starts jobs and tracks the running ones.
type Runner struct {
	dir     string
	mu      sync.Mutex
	running map[string]*run
}

type run struct {
	cmd       *exec.Cmd
	cancelled bool
	done      chan struct{}
}

// NewRunner returns a runner that keeps job output under dir.
func NewRunner(dir string) *Runner {
	return &Runner{dir: dir, running: map[string]*run{}}
}

// Start starts job id. done is called once, from another goroutine, after the
// process ended. An error means the process did not start and done is not
// called.
func (r *Runner) Start(id string, s core.JobSpec, done func(core.JobExit)) error {
	outDir := filepath.Join(r.dir, id)
	if err := os.MkdirAll(outDir, 0o700); err != nil {
		return err
	}
	stdout, err := os.OpenFile(filepath.Join(outDir, "stdout"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	stderr, err := os.OpenFile(filepath.Join(outDir, "stderr"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		stdout.Close()
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), s.Timeout)
	cmd := command.Command(ctx, s.Executable, s.Args, s.Dir, command.Env(nil))
	out, errOut := &command.LimitedWriter{W: stdout, N: OutputLimit}, &command.LimitedWriter{W: stderr, N: OutputLimit}
	cmd.Stdout, cmd.Stderr = out, errOut
	if err := cmd.Start(); err != nil {
		cancel()
		stdout.Close()
		stderr.Close()
		return fmt.Errorf("start: %w", err)
	}
	job := &run{cmd: cmd, done: make(chan struct{})}
	r.mu.Lock()
	r.running[id] = job
	r.mu.Unlock()

	go func() {
		defer close(job.done)
		defer cancel()
		waitErr := cmd.Wait()
		command.KillGroup(cmd)
		stdout.Close()
		stderr.Close()
		r.mu.Lock()
		delete(r.running, id)
		cancelled := job.cancelled
		r.mu.Unlock()

		exit := core.JobExit{
			TimedOut:  errors.Is(ctx.Err(), context.DeadlineExceeded),
			Cancelled: cancelled,
			Truncated: out.Truncated || errOut.Truncated,
		}
		if code := cmd.ProcessState.ExitCode(); code >= 0 {
			exit.ExitCode = &code
		}
		var exitErr *exec.ExitError
		if waitErr != nil && !errors.As(waitErr, &exitErr) {
			exit.Err = waitErr.Error()
		}
		done(exit)
	}()
	return nil
}

// Cancel kills a running job and returns after its done callback returned.
func (r *Runner) Cancel(id string) error {
	r.mu.Lock()
	job, ok := r.running[id]
	if ok {
		job.cancelled = true
	}
	r.mu.Unlock()
	if !ok {
		return core.ErrJobNotRunning
	}
	if err := command.KillGroup(job.cmd); err != nil {
		return err
	}
	<-job.done
	return nil
}

// ReadOutput reads up to limit bytes of a stream ("stdout" or "stderr").
func (r *Runner) ReadOutput(id, stream string, offset int64, limit int) (*core.JobOutput, error) {
	if stream != "stdout" && stream != "stderr" {
		return nil, fmt.Errorf("unknown stream %q", stream)
	}
	f, err := os.Open(filepath.Join(r.dir, id, stream))
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
	return &core.JobOutput{Data: buf[:n], Offset: offset, Size: info.Size()}, nil
}
