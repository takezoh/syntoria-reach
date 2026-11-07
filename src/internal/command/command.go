// Package command starts executables without a shell. Every child runs in its
// own process group, gets a minimal environment, and is killed when the
// daemon dies, so no work outlives the daemon unobserved.
package command

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"sort"
	"syscall"
	"time"

	"github.com/takezoh/syntoria-reach/src/internal/core"
)

// Env returns the whole environment of a child: a fixed PATH, LANG and HOME
// plus extra. The daemon's own environment is not inherited.
func Env(extra map[string]string) []string {
	e := []string{"PATH=/usr/local/bin:/usr/bin:/bin", "LANG=C.UTF-8"}
	if home, err := os.UserHomeDir(); err == nil {
		e = append(e, "HOME="+home)
	}
	keys := make([]string, 0, len(extra))
	for k := range extra {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		e = append(e, k+"="+extra[k])
	}
	return e
}

// Command prepares a child that runs in its own process group, is killed as a
// group when ctx ends, and receives SIGKILL if the daemon dies.
func Command(ctx context.Context, executable string, args []string, dir string, env []string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, executable, args...)
	cmd.Dir = dir
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
	cmd.Cancel = func() error { return KillGroup(cmd) }
	cmd.WaitDelay = 5 * time.Second
	return cmd
}

// KillGroup kills the started command's whole process group.
func KillGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}

// Run starts the process and waits for it. A non-zero exit or a timeout is a
// Result, not an error; an error means the process could not be started.
func Run(ctx context.Context, s core.CommandSpec) (*core.CommandResult, error) {
	ctx, cancel := context.WithTimeout(ctx, s.Timeout)
	defer cancel()

	cmd := Command(ctx, s.Executable, s.Args, s.Dir, Env(nil))
	var stdoutBuf, stderrBuf bytes.Buffer
	stdout := &LimitedWriter{W: &stdoutBuf, N: int64(s.OutputLimit)}
	stderr := &LimitedWriter{W: &stderrBuf, N: int64(s.OutputLimit)}
	cmd.Stdout, cmd.Stderr = stdout, stderr

	start := time.Now()
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	err := cmd.Wait()
	KillGroup(cmd) // children left behind by the main process
	res := &core.CommandResult{
		ExitCode:   cmd.ProcessState.ExitCode(),
		Stdout:     stdoutBuf.Bytes(),
		Stderr:     stderrBuf.Bytes(),
		Truncated:  stdout.Truncated || stderr.Truncated,
		TimedOut:   errors.Is(ctx.Err(), context.DeadlineExceeded),
		DurationMS: time.Since(start).Milliseconds(),
	}
	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) && !errors.Is(err, exec.ErrWaitDelay) {
		return res, err
	}
	return res, nil
}

// LimitedWriter passes the first N bytes to W and discards the rest, setting
// Truncated. Discarded bytes are reported as written, so a child is not
// stopped by a full log. It must not expose ReadFrom, or io.Copy would bypass
// the limit.
type LimitedWriter struct {
	W         io.Writer
	N         int64
	Truncated bool
	written   int64
}

func (w *LimitedWriter) Write(p []byte) (int, error) {
	n := len(p)
	if room := w.N - w.written; room < int64(len(p)) {
		p = p[:max(room, 0)]
		w.Truncated = true
	}
	m, err := w.W.Write(p)
	w.written += int64(m)
	if err != nil {
		return m, err
	}
	return n, nil
}
