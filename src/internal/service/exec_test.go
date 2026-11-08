package service_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/takezoh/syntoria-reach/src/internal/core"
	"github.com/takezoh/syntoria-reach/src/internal/service"
)

func (f *fixture) startJob(t *testing.T, opID string) *core.Job {
	t.Helper()
	op, err := f.svc.StartJob(context.Background(), f.a, service.StartJobRequest{OperationID: opID, Workspace: f.ws, Executable: "/bin/sh", Args: []string{"-c", "x"}})
	if err != nil || op.State != core.StateSucceeded {
		t.Fatalf("start job: %+v %v", op, err)
	}
	j := result[core.Job](t, op)
	return &j
}

func (f *fixture) getJob(t *testing.T, id string) *core.Job {
	t.Helper()
	j, err := f.svc.GetJob(f.a, service.JobRequest{JobID: id})
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func TestStartJob(t *testing.T) {
	f := newFixture(t)
	if _, err := f.svc.StartJob(context.Background(), f.b, service.StartJobRequest{OperationID: "j", Workspace: f.ws, Executable: "/bin/sh"}); code(err) != core.CodePermissionDenied {
		t.Fatalf("client without command.run: %v", err)
	}
	if _, err := f.svc.StartJob(context.Background(), f.a, service.StartJobRequest{OperationID: "j", Workspace: f.ws, Executable: "/bin/bash"}); code(err) != core.CodePermissionDenied {
		t.Fatalf("executable not in allowlist: %v", err)
	}
	if _, err := f.svc.StartJob(context.Background(), f.a, service.StartJobRequest{OperationID: "j", Workspace: f.ws, Executable: "/bin/sh", TimeoutSeconds: 24*3600 + 1}); code(err) != core.CodeInvalidRequest {
		t.Fatalf("timeout out of range: %v", err)
	}

	j := f.startJob(t, "j1")
	if j.State != core.JobRunning || j.WorkspaceID != f.ws || j.Executable != "/bin/sh" {
		t.Fatalf("job = %+v", j)
	}
	spec, ok := f.fakes.Jobs.Spec(j.ID)
	if !ok || spec.Dir != f.root || spec.Timeout != time.Hour || strings.Join(spec.Args, " ") != "-c x" {
		t.Fatalf("spec = %+v", spec)
	}
	if again := f.startJob(t, "j1"); again.ID != j.ID {
		t.Fatal("resend started another job")
	}
	if _, err := f.svc.GetJob(f.b, service.JobRequest{JobID: j.ID}); code(err) != core.CodeNotFound {
		t.Fatalf("other client sees job: %v", err)
	}
}

func TestJobOutcomeIsRecorded(t *testing.T) {
	zero, two := 0, 2
	cases := map[string]struct {
		exit  core.JobExit
		state string
		msg   string
	}{
		"success":  {core.JobExit{ExitCode: &zero, Truncated: true}, core.JobSucceeded, ""},
		"failure":  {core.JobExit{ExitCode: &two}, core.JobFailed, ""},
		"timeout":  {core.JobExit{TimedOut: true}, core.JobFailed, "timed out"},
		"signaled": {core.JobExit{}, core.JobFailed, "terminated by a signal"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			j := f.startJob(t, "j")
			if err := f.fakes.Jobs.Exit(j.ID, c.exit); err != nil {
				t.Fatal(err)
			}
			got := f.getJob(t, j.ID)
			if got.State != c.state || got.ErrorMessage != c.msg || got.TimedOut != c.exit.TimedOut || got.Truncated != c.exit.Truncated ||
				(c.exit.ExitCode == nil) != (got.ExitCode == nil) {
				t.Fatalf("job = %+v", got)
			}
		})
	}
}

func TestJobStartFailure(t *testing.T) {
	f := newFixture(t)
	f.fakes.Jobs.StartErr = errors.New("start: no such file")
	j := f.startJob(t, "j")
	if j.State != core.JobFailed || j.ErrorMessage != "start: no such file" {
		t.Fatalf("job = %+v", j)
	}
	// A job that never ran does not keep the workspace busy.
	if busy, err := f.fakes.Store.WorkspaceBusy(f.ws); busy || err != nil {
		t.Fatalf("busy=%v err=%v", busy, err)
	}
}

func TestCancelAndReadJobOutput(t *testing.T) {
	f := newFixture(t)
	j := f.startJob(t, "j")
	f.fakes.Jobs.SetOutput(j.ID, "stdout", []byte("started\n"))

	// While the job runs, reaching the end of the output is not EOF.
	out, err := f.svc.ReadJobOutput(f.a, service.ReadJobOutputRequest{JobID: j.ID, Stream: "stdout"})
	if err != nil || string(out.Data) != "started\n" || out.EOF || out.State != core.JobRunning {
		t.Fatalf("output = %+v %v", out, err)
	}
	if _, err := f.svc.ReadJobOutput(f.a, service.ReadJobOutputRequest{JobID: j.ID, Stream: "log"}); code(err) != core.CodeInvalidRequest {
		t.Fatalf("unknown stream: %v", err)
	}
	if _, err := f.svc.CancelJob(f.b, service.CancelJobRequest{OperationID: "c", JobID: j.ID}); code(err) != core.CodeNotFound {
		t.Fatalf("other client cancels: %v", err)
	}

	op, err := f.svc.CancelJob(f.a, service.CancelJobRequest{OperationID: "c1", JobID: j.ID})
	if err != nil || result[core.Job](t, op).State != core.JobCancelled {
		t.Fatalf("cancel: %+v %v", op, err)
	}
	out, _ = f.svc.ReadJobOutput(f.a, service.ReadJobOutputRequest{JobID: j.ID, Stream: "stdout", Offset: 4})
	if string(out.Data) != "ted\n" || !out.EOF || out.State != core.JobCancelled {
		t.Fatalf("output after cancel = %+v", out)
	}
	// Cancelling an ended job succeeds and leaves it unchanged.
	if op, err = f.svc.CancelJob(f.a, service.CancelJobRequest{OperationID: "c2", JobID: j.ID}); err != nil || op.State != core.StateSucceeded || result[core.Job](t, op).State != core.JobCancelled {
		t.Fatalf("second cancel: %+v %v", op, err)
	}
	// A late exit report does not overwrite the outcome.
	f.fakes.Jobs.Exit(j.ID, core.JobExit{})
	if got := f.getJob(t, j.ID); got.State != core.JobCancelled {
		t.Fatalf("job = %+v", got)
	}
}
