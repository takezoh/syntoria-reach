package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/takezoh/syntoria-reach/src/internal/agent/agenttest"
	"github.com/takezoh/syntoria-reach/src/internal/config"
	"github.com/takezoh/syntoria-reach/src/internal/orca/orcatest"
	"github.com/takezoh/syntoria-reach/src/internal/store"
)

const (
	tokenA = "token-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	tokenB = "token-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

type fixture struct {
	svc      *Service
	st       *store.Store
	db       string
	orcaRoot string
	ws       string // a workspace of client a
	root     string // its path
	a, b     *Client
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	orcaRoot := filepath.Join(dir, "orca")
	if err := os.Mkdir(orcaRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(dir, "reach.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	agentCmd, agentEnv := agenttest.Agent(t, config.ProtocolCodex, "TestFakeHarness")
	cfg := &config.Config{
		Database: filepath.Join(dir, "reach.db"),
		Orca:     orcatest.Command(t, "TestFakeHarness", orcaRoot),
		Agents:   []config.Agent{{ID: "codex", Protocol: config.ProtocolCodex, Command: agentCmd, Env: agentEnv, Sandbox: "workspace-write"}},
		Projects: []config.Project{{ID: "proj", Repo: "/repo", Commands: []string{"/bin/sh"}, Agents: []string{"codex"}}},
		Clients: []config.Client{
			{ID: "a", Token: tokenA, Grants: []config.Grant{{Project: "proj", Allow: []string{
				config.PermWorkspaceCreate, config.PermFileRead, config.PermFileWrite, config.PermCommandRun, config.PermAgentRun}}}},
			{ID: "b", Token: tokenB, Grants: []config.Grant{{Project: "proj", Allow: []string{config.PermWorkspaceCreate, config.PermFileRead}}}},
		},
	}
	svc, err := New(cfg, st)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(svc.Close)
	a, _ := svc.Authenticate(tokenA)
	b, _ := svc.Authenticate(tokenB)
	f := &fixture{svc: svc, st: st, db: filepath.Join(dir, "reach.db"), orcaRoot: orcaRoot, a: a, b: b}
	f.ws = f.newWorkspace(t, a, "ws")
	f.root = filepath.Join(orcaRoot, "ws")
	return f
}

// newWorkspace creates a workspace for c and returns its ID.
func (f *fixture) newWorkspace(t *testing.T, c *Client, name string) string {
	t.Helper()
	op, err := f.svc.CreateWorkspace(context.Background(), c, CreateWorkspaceRequest{OperationID: "create-" + name, Project: "proj", Name: name})
	if err != nil || op.State != store.StateSucceeded {
		t.Fatalf("create workspace: %+v %v", op, err)
	}
	var w store.Workspace
	json.Unmarshal(op.Result, &w)
	return w.ID
}

func code(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

func TestAuthenticate(t *testing.T) {
	f := newFixture(t)
	if f.a.ID != "a" || f.b.ID != "b" {
		t.Fatal("wrong client")
	}
	if _, err := f.svc.Authenticate("wrong"); code(err) != CodeUnauthorized {
		t.Fatalf("err = %v", err)
	}
}

func TestRunCommandIsIdempotent(t *testing.T) {
	f := newFixture(t)
	req := RunCommandRequest{OperationID: "op-1", Workspace: f.ws, Executable: "/bin/sh", Args: []string{"-c", "echo x >> count; cat count"}}
	op1, err := f.svc.RunCommand(context.Background(), f.a, req)
	if err != nil || op1.State != store.StateSucceeded {
		t.Fatalf("op=%+v err=%v", op1, err)
	}
	op2, err := f.svc.RunCommand(context.Background(), f.a, req)
	if err != nil || string(op2.Result) != string(op1.Result) {
		t.Fatalf("replay: op=%+v err=%v", op2, err)
	}
	if b, _ := os.ReadFile(filepath.Join(f.root, "count")); string(b) != "x\n" {
		t.Fatalf("command ran more than once: %q", b)
	}

	req.Args = []string{"-c", "echo different"}
	if _, err := f.svc.RunCommand(context.Background(), f.a, req); code(err) != CodeOperationConflict {
		t.Fatalf("conflict err = %v", err)
	}
	op, err := f.svc.GetOperation(f.a, GetOperationRequest{OperationID: "op-1"})
	if err != nil || op.State != store.StateSucceeded {
		t.Fatalf("get_operation: %+v %v", op, err)
	}
	// Operation IDs are scoped per client.
	if _, err := f.svc.GetOperation(f.b, GetOperationRequest{OperationID: "op-1"}); code(err) != CodeNotFound {
		t.Fatalf("other client sees operation: %v", err)
	}
}

func TestPermissions(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if _, err := f.svc.RunCommand(ctx, f.b, RunCommandRequest{OperationID: "x", Workspace: f.ws, Executable: "/bin/sh"}); code(err) != CodePermissionDenied {
		t.Fatalf("client without command.run: %v", err)
	}
	if _, err := f.svc.RunCommand(ctx, f.a, RunCommandRequest{OperationID: "x", Workspace: f.ws, Executable: "/bin/bash"}); code(err) != CodePermissionDenied {
		t.Fatalf("executable not in allowlist: %v", err)
	}
	if _, err := f.svc.WriteFile(f.b, WriteFileRequest{OperationID: "x", Workspace: f.ws, Path: "f", Content: []byte("x")}); code(err) != CodePermissionDenied {
		t.Fatalf("client without file.write: %v", err)
	}
	if _, err := f.svc.ReadFile(f.a, ReadFileRequest{Workspace: "nope", Path: "f"}); code(err) != CodePermissionDenied {
		t.Fatalf("unknown workspace: %v", err)
	}
	if _, err := f.svc.ReadFile(f.a, ReadFileRequest{Workspace: f.ws, Path: "../x"}); code(err) != CodeInvalidRequest {
		t.Fatalf("path outside: %v", err)
	}
	// Denied requests must not create operations.
	if _, err := f.st.GetOperation("b", "x"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("denied request was admitted: %v", err)
	}
}

func TestWriteFileFailureIsRecorded(t *testing.T) {
	f := newFixture(t)
	req := WriteFileRequest{OperationID: "w1", Workspace: f.ws, Path: "f.txt", Content: []byte("one")}
	op, err := f.svc.WriteFile(f.a, req)
	if err != nil || op.State != store.StateSucceeded {
		t.Fatalf("op=%+v err=%v", op, err)
	}
	var res WriteFileResult
	json.Unmarshal(op.Result, &res)

	// Creating again without a version fails, and the failure is stored.
	op, err = f.svc.WriteFile(f.a, WriteFileRequest{OperationID: "w2", Workspace: f.ws, Path: "f.txt", Content: []byte("two")})
	if err != nil || op.State != store.StateFailed || op.ErrorCode != CodeVersionConflict {
		t.Fatalf("op=%+v err=%v", op, err)
	}
	op, err = f.svc.WriteFile(f.a, WriteFileRequest{OperationID: "w3", Workspace: f.ws, Path: "f.txt", Content: []byte("two"), ExpectedVersion: res.Version})
	if err != nil || op.State != store.StateSucceeded {
		t.Fatalf("op=%+v err=%v", op, err)
	}
}

func TestAuditHasNoContent(t *testing.T) {
	f := newFixture(t)
	f.svc.WriteFile(f.a, WriteFileRequest{OperationID: "w1", Workspace: f.ws, Path: "f.txt", Content: []byte("TOPSECRET")})
	f.svc.RunCommand(context.Background(), f.b, RunCommandRequest{OperationID: "c1", Workspace: f.ws, Executable: "/bin/sh"})

	db, err := sql.Open("sqlite", f.db)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT client_id, action, target, outcome, error_code FROM audit_records
		WHERE action != 'create_workspace' ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var c, a, tg, o, e string
		rows.Scan(&c, &a, &tg, &o, &e)
		got = append(got, strings.Join([]string{c, a, tg, o, e}, "|"))
	}
	want := []string{"a|write_file|f.txt|ok|", "b|run_command|/bin/sh|denied|permission_denied"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("audit = %v", got)
	}
}
