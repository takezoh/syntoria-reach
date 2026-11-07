// Package command runs a short-lived executable without a shell.
package command

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// Spec describes one execution. Executable must be an absolute path and Args
// are passed verbatim (no shell expansion).
type Spec struct {
	Executable  string
	Args        []string
	Dir         string
	Timeout     time.Duration
	OutputLimit int // per stream, in bytes
}

// Result is the observed outcome of a finished process.
type Result struct {
	ExitCode   int    `json:"exit_code"`
	Stdout     []byte `json:"stdout"`
	Stderr     []byte `json:"stderr"`
	Truncated  bool   `json:"truncated"`
	TimedOut   bool   `json:"timed_out"`
	DurationMS int64  `json:"duration_ms"`
}

// env is the whole environment of a command; the daemon's own environment is
// not inherited.
func env() []string {
	e := []string{"PATH=/usr/local/bin:/usr/bin:/bin", "LANG=C.UTF-8"}
	if home, err := os.UserHomeDir(); err == nil {
		e = append(e, "HOME="+home)
	}
	return e
}

// Run starts the process and waits for it. A non-zero exit or a timeout is a
// Result, not an error; an error means the process could not be started.
func Run(ctx context.Context, s Spec) (*Result, error) {
	ctx, cancel := context.WithTimeout(ctx, s.Timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, s.Executable, s.Args...)
	cmd.Dir = s.Dir
	cmd.Env = env()
	cmd.Stdin = nil // /dev/null
	stdout := &limitedBuffer{limit: s.OutputLimit}
	stderr := &limitedBuffer{limit: s.OutputLimit}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	// Run in its own process group so a timeout kills its children too.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 5 * time.Second

	start := time.Now()
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	err := cmd.Wait()
	res := &Result{
		ExitCode:   cmd.ProcessState.ExitCode(),
		Stdout:     stdout.Bytes(),
		Stderr:     stderr.Bytes(),
		Truncated:  stdout.truncated || stderr.truncated,
		TimedOut:   errors.Is(ctx.Err(), context.DeadlineExceeded),
		DurationMS: time.Since(start).Milliseconds(),
	}
	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) && !errors.Is(err, exec.ErrWaitDelay) {
		return res, err
	}
	return res, nil
}

// limitedBuffer keeps the first limit bytes written to it. It must not expose
// ReadFrom, or io.Copy would bypass the limit.
type limitedBuffer struct {
	buf       bytes.Buffer
	limit     int
	truncated bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if room := b.limit - b.buf.Len(); room < len(p) {
		p = p[:max(room, 0)]
		b.truncated = true
	}
	b.buf.Write(p)
	return n, nil
}

func (b *limitedBuffer) Bytes() []byte { return b.buf.Bytes() }
