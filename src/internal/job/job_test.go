package job

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/takezoh/syntoria-reach/src/internal/core"
)

// start runs script with /bin/sh and returns a channel with its exit.
func start(t *testing.T, r *Runner, id, script string, timeout time.Duration) <-chan core.JobExit {
	t.Helper()
	exits := make(chan core.JobExit, 1)
	err := r.Start(id, core.JobSpec{Dir: t.TempDir(), Executable: "/bin/sh", Args: []string{"-c", script}, Timeout: timeout},
		func(e core.JobExit) { exits <- e })
	if err != nil {
		t.Fatal(err)
	}
	return exits
}

func wait(t *testing.T, exits <-chan core.JobExit) core.JobExit {
	t.Helper()
	select {
	case e := <-exits:
		return e
	case <-time.After(10 * time.Second):
		t.Fatal("job did not finish")
	}
	return core.JobExit{}
}

func TestOutputAndExit(t *testing.T) {
	r := NewRunner(filepath.Join(t.TempDir(), "jobs"))
	e := wait(t, start(t, r, "j1", "echo out; echo err >&2; exit 4", time.Minute))
	if e.ExitCode == nil || *e.ExitCode != 4 || e.Cancelled || e.TimedOut || e.Err != "" {
		t.Fatalf("exit = %+v", e)
	}
	out, err := r.ReadOutput("j1", "stdout", 0, 100)
	if err != nil || string(out.Data) != "out\n" || out.Size != 4 {
		t.Fatalf("stdout = %+v %v", out, err)
	}
	out, _ = r.ReadOutput("j1", "stderr", 1, 100)
	if string(out.Data) != "rr\n" {
		t.Fatalf("stderr from offset 1 = %q", out.Data)
	}
	if _, err := r.ReadOutput("j1", "../x", 0, 1); err == nil {
		t.Fatal("unknown stream accepted")
	}
}

func TestCancelAndTimeout(t *testing.T) {
	r := NewRunner(filepath.Join(t.TempDir(), "jobs"))
	exits := start(t, r, "j1", "sleep 30", time.Minute)
	if err := r.Cancel("j1"); err != nil {
		t.Fatal(err)
	}
	if e := wait(t, exits); !e.Cancelled || e.ExitCode != nil {
		t.Fatalf("after cancel: %+v", e)
	}
	if err := r.Cancel("j1"); err != core.ErrJobNotRunning {
		t.Fatalf("second cancel err = %v", err)
	}
	if e := wait(t, start(t, r, "j2", "sleep 30", 200*time.Millisecond)); !e.TimedOut {
		t.Fatalf("after timeout: %+v", e)
	}
}

func TestStartFailure(t *testing.T) {
	r := NewRunner(filepath.Join(t.TempDir(), "jobs"))
	err := r.Start("j1", core.JobSpec{Dir: t.TempDir(), Executable: "/nonexistent", Timeout: time.Minute},
		func(core.JobExit) { t.Error("done called for a job that did not start") })
	if err == nil {
		t.Fatal("start succeeded")
	}
}
