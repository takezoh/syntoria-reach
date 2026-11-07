package job

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/takezoh/syntoria-reach/src/internal/store"
)

func newManager(t *testing.T) (*Manager, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "reach.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return New(st, filepath.Join(dir, "jobs")), st
}

func startSh(t *testing.T, m *Manager, script string, timeout time.Duration) *store.Job {
	t.Helper()
	j, err := m.Start(Spec{ClientID: "c", WorkspaceID: "ws", Dir: t.TempDir(), Executable: "/bin/sh", Args: []string{"-c", script}, Timeout: timeout})
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func wait(t *testing.T, st *store.Store, id string) *store.Job {
	t.Helper()
	for range 500 {
		j, err := st.GetJob("c", id)
		if err != nil {
			t.Fatal(err)
		}
		if j.State != store.JobRunning {
			return j
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("job did not finish")
	return nil
}

func TestJobOutputAndExit(t *testing.T) {
	m, st := newManager(t)
	j := startSh(t, m, "echo out; echo err >&2; exit 4", time.Minute)
	if j.State != store.JobRunning {
		t.Fatalf("state = %s", j.State)
	}
	j = wait(t, st, j.ID)
	if j.State != store.JobFailed || j.ExitCode == nil || *j.ExitCode != 4 {
		t.Fatalf("job = %+v", j)
	}
	out, err := m.ReadOutput(j.ID, "stdout", 0, 100)
	if err != nil || string(out.Data) != "out\n" || out.Size != 4 {
		t.Fatalf("stdout = %+v %v", out, err)
	}
	out, _ = m.ReadOutput(j.ID, "stderr", 1, 100)
	if string(out.Data) != "rr\n" {
		t.Fatalf("stderr from offset 1 = %q", out.Data)
	}
	if _, err := m.ReadOutput(j.ID, "../x", 0, 1); err == nil {
		t.Fatal("unknown stream accepted")
	}
}

func TestJobCancelAndTimeout(t *testing.T) {
	m, st := newManager(t)
	j := startSh(t, m, "sleep 30", time.Minute)
	if err := m.Cancel(j.ID); err != nil {
		t.Fatal(err)
	}
	if j, _ = st.GetJob("c", j.ID); j.State != store.JobCancelled {
		t.Fatalf("after cancel: %+v", j)
	}
	if err := m.Cancel(j.ID); err != ErrNotRunning {
		t.Fatalf("second cancel err = %v", err)
	}

	j = startSh(t, m, "sleep 30", 200*time.Millisecond)
	if j = wait(t, st, j.ID); j.State != store.JobFailed || !j.TimedOut {
		t.Fatalf("after timeout: %+v", j)
	}
}

func TestJobStartFailure(t *testing.T) {
	m, _ := newManager(t)
	j, err := m.Start(Spec{ClientID: "c", WorkspaceID: "ws", Dir: t.TempDir(), Executable: "/nonexistent", Timeout: time.Minute})
	if err != nil || j.State != store.JobFailed || j.ErrorMessage == "" {
		t.Fatalf("job=%+v err=%v", j, err)
	}
}
