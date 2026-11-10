package agent

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"sync"

	"github.com/takezoh/syntoria-reach/src/internal/command"
)

// process is one harness child process: its process group, stdio pipes,
// private stderr log, cancellation and one-time reaping. Its lifetime is detached from any request so
// protocol cleanup can still run after a caller goes away.
type process struct {
	cmd    *exec.Cmd
	cancel context.CancelFunc
	stdin  io.WriteCloser
	stdout io.ReadCloser
	log    *os.File

	waitOnce  sync.Once
	waited    chan struct{}
	waitErr   error
	closeOnce sync.Once
	closeErr  error
}

// startProcess starts argv in dir with env, writing stderr (bounded) to a
// new file at logPath.
func startProcess(argv []string, dir string, env []string, logPath string) (*process, error) {
	if len(argv) == 0 {
		return nil, errors.New("empty harness command")
	}
	life, cancel := context.WithCancel(context.Background())
	cmd := command.Command(life, argv[0], argv[1:], dir, env)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	log, err := os.OpenFile(logPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		cancel()
		return nil, err
	}
	cmd.Stderr = &command.LimitedWriter{W: log, N: stderrLimit}
	if err := cmd.Start(); err != nil {
		log.Close()
		cancel()
		return nil, err
	}
	return &process{cmd: cmd, cancel: cancel, stdin: stdin, stdout: stdout, log: log, waited: make(chan struct{})}, nil
}

// wait waits until the protocol reader has stopped, then reaps the child once
// and kills anything left in its process group. Concurrent callers share the
// result.
func (p *process) wait(protocolDone <-chan struct{}) error {
	if protocolDone != nil {
		<-protocolDone
	}
	p.waitOnce.Do(func() {
		p.waitErr = p.cmd.Wait()
		command.KillGroup(p.cmd)
		p.log.Close()
		close(p.waited)
	})
	<-p.waited
	return p.waitErr
}

// close asks the protocol to close with ctx, then kills the process group and
// reaps the child.
func (p *process) close(ctx context.Context, h harness) error {
	p.closeOnce.Do(func() {
		if h != nil {
			p.closeErr = h.close(ctx)
		}
		p.cancel()
		var done <-chan struct{}
		if h != nil {
			done = h.done()
		}
		p.wait(done)
	})
	return p.closeErr
}
