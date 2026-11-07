package store

import (
	"path/filepath"
	"testing"
)

func TestAdmitAndRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reach.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	op, created, err := s.Admit("c", "op", "run_command", "h1")
	if err != nil || !created || op.State != StateRunning {
		t.Fatalf("op=%+v created=%v err=%v", op, created, err)
	}
	if op, created, _ = s.Admit("c", "op", "run_command", "h2"); created || op.ArgsHash != "h1" {
		t.Fatalf("duplicate admission: %+v", op)
	}
	if _, created, _ = s.Admit("other", "op", "run_command", "h1"); !created {
		t.Fatal("operation IDs must be scoped per client")
	}
	s.Close()

	// A daemon restart must not leave the operation running or rerun it.
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if n, err := s.MarkRunningUnknown(); err != nil || n != 2 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	op, err = s.GetOperation("c", "op")
	if err != nil || op.State != StateUnknown || op.ErrorCode != "result_unknown" {
		t.Fatalf("op=%+v err=%v", op, err)
	}
	// A late Finish cannot overwrite an unknown outcome.
	if op, _ = s.Finish("c", "op", StateSucceeded, nil, "", ""); op.State != StateUnknown {
		t.Fatalf("state = %s", op.State)
	}
}
