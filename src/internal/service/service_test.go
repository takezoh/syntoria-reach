package service_test

// The service runs against the fakes of its ports, with the Orca workspace
// backend (orcaws) on a fake Orca CLI, which covers what Reach does with
// Orca. What the adapters do with the operating system, SQLite and agent
// processes, and the local workspace backend, are covered by their own tests
// (including the shared contracts in servicetest) and by the end-to-end
// tests in cmd/reach.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/takezoh/syntoria-reach/src/internal/config"
	"github.com/takezoh/syntoria-reach/src/internal/core"
	"github.com/takezoh/syntoria-reach/src/internal/orcaws"
	"github.com/takezoh/syntoria-reach/src/internal/service"
	"github.com/takezoh/syntoria-reach/src/internal/service/servicetest"
)

const (
	tokenA = "token-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	tokenB = "token-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	tokenC = "token-cccccccccccccccccccccccccccccccc"
)

type fixture struct {
	svc     *service.Service
	fakes   *servicetest.Fakes
	a, b, c *service.Client // a: everything; b: read only; c: agents only
	ws      string          // a workspace, created by client a
	root    string          // its path
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	cfg := &config.Config{
		Agents: []config.Agent{
			{ID: "codex", Role: "Writes code", Command: []string{"/bin/codex", "--model", "gpt x"}},
			{ID: "dsh", Role: "Reviews code", Command: []string{"/bin/dsh"}},
		},
		Workspaces: "/w",
		Commands:   []string{"/bin/sh"},
		Clients: []config.Client{
			{ID: "a", Token: tokenA, Allow: []string{core.PermWorkspaceCreate, core.PermFileRead, core.PermFileWrite, core.PermCommandRun, core.PermAgentRun}},
			{ID: "b", Token: tokenB, Allow: []string{core.PermWorkspaceCreate, core.PermFileRead}},
			{ID: "c", Token: tokenC, Allow: []string{core.PermWorkspaceCreate, core.PermAgentRun}},
		},
	}
	fakes := servicetest.New()
	svc := service.New(cfg, fakes.Deps(orcaws.New(fakes.Orca, fakes.Folders, cfg.Workspaces)))
	t.Cleanup(svc.Close)
	f := &fixture{svc: svc, fakes: fakes}
	f.a, _ = svc.Authenticate(tokenA)
	f.b, _ = svc.Authenticate(tokenB)
	f.c, _ = svc.Authenticate(tokenC)
	f.ws = f.newWorkspace(t, f.a, "ws")
	f.root = "/w/reach-ws"
	return f
}

// newWorkspace creates a workspace for c and returns its ID.
func (f *fixture) newWorkspace(t *testing.T, c *service.Client, name string) string {
	t.Helper()
	op, err := f.svc.CreateWorkspace(context.Background(), c, service.CreateWorkspaceRequest{OperationID: "create-" + name, Name: name, Note: "work on " + name})
	if err != nil || op.State != core.StateSucceeded {
		t.Fatalf("create workspace: %+v %v", op, err)
	}
	return result[core.Workspace](t, op).ID
}

// result decodes an operation's result.
func result[T any](t *testing.T, op *core.Operation) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(op.Result, &v); err != nil {
		t.Fatalf("result of %+v: %v", op, err)
	}
	return v
}

func code(err error) string {
	var e *core.Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

// audit returns the audit log as "client|action|target|outcome|code" lines,
// leaving out the fixture's workspace creation.
func (f *fixture) audit() []string {
	var out []string
	for _, r := range f.fakes.Store.AuditRecords() {
		if r.OperationID == "create-ws" {
			continue
		}
		out = append(out, strings.Join([]string{r.ClientID, r.Action, r.Target, r.Outcome, r.ErrorCode}, "|"))
	}
	return out
}

func TestAuthenticate(t *testing.T) {
	f := newFixture(t)
	if f.a.ID != "a" || f.b.ID != "b" {
		t.Fatal("wrong client")
	}
	if _, err := f.svc.Authenticate("wrong"); code(err) != core.CodeUnauthorized {
		t.Fatalf("err = %v", err)
	}
}

func TestRunCommandIsIdempotent(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	req := service.RunCommandRequest{OperationID: "op-1", Workspace: f.ws, Executable: "/bin/sh", Args: []string{"-c", "true"}}
	op1, err := f.svc.RunCommand(ctx, f.a, req)
	if err != nil || op1.State != core.StateSucceeded || result[core.CommandResult](t, op1).ExitCode != 0 {
		t.Fatalf("op=%+v err=%v", op1, err)
	}
	op2, err := f.svc.RunCommand(ctx, f.a, req)
	if err != nil || string(op2.Result) != string(op1.Result) {
		t.Fatalf("replay: op=%+v err=%v", op2, err)
	}
	calls := f.fakes.Commands.Calls()
	if len(calls) != 1 {
		t.Fatalf("command ran %d times", len(calls))
	}
	if c := calls[0]; c.Dir != f.root || c.Timeout.Seconds() != 60 || c.OutputLimit != 1<<20 {
		t.Fatalf("spec = %+v", c)
	}

	req.Args = []string{"-c", "echo different"}
	if _, err := f.svc.RunCommand(ctx, f.a, req); code(err) != core.CodeOperationConflict {
		t.Fatalf("conflict err = %v", err)
	}
	if op, err := f.svc.GetOperation(f.a, service.GetOperationRequest{OperationID: "op-1"}); err != nil || op.State != core.StateSucceeded {
		t.Fatalf("get_operation: %+v %v", op, err)
	}
	// Operation IDs are scoped per client.
	if _, err := f.svc.GetOperation(f.b, service.GetOperationRequest{OperationID: "op-1"}); code(err) != core.CodeNotFound {
		t.Fatalf("other client sees operation: %v", err)
	}
	if _, err := f.svc.RunCommand(ctx, f.a, service.RunCommandRequest{OperationID: "bad id", Workspace: f.ws, Executable: "/bin/sh"}); code(err) != core.CodeInvalidRequest {
		t.Fatalf("invalid operation ID: %v", err)
	}
	if got, want := f.audit(), []string{
		"a|run_command|/bin/sh|ok|",
		"a|run_command|/bin/sh|replayed|",
		"a|run_command|/bin/sh|error|operation_conflict",
		"a|get_operation||ok|",
		"b|get_operation||error|not_found",
		"a|run_command|/bin/sh|error|invalid_request",
	}; strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("audit = %q", got)
	}
}

func TestCommandStartFailureIsRecorded(t *testing.T) {
	f := newFixture(t)
	f.fakes.Commands.Result = func(core.CommandSpec) (*core.CommandResult, error) { return nil, errors.New("exec format error") }
	op, err := f.svc.RunCommand(context.Background(), f.a, service.RunCommandRequest{OperationID: "op", Workspace: f.ws, Executable: "/bin/sh"})
	if err != nil || op.State != core.StateFailed || op.ErrorCode != core.CodeInvalidRequest || !strings.Contains(op.ErrorMessage, "exec format error") {
		t.Fatalf("op=%+v err=%v", op, err)
	}
	if got := f.audit(); len(got) != 1 || got[0] != "a|run_command|/bin/sh|failed|invalid_request" {
		t.Fatalf("audit = %q", got)
	}
}

func TestPermissions(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	cases := map[string]struct {
		call func() error
		code string
	}{
		"client without command.run": {func() error {
			_, err := f.svc.RunCommand(ctx, f.b, service.RunCommandRequest{OperationID: "x", Workspace: f.ws, Executable: "/bin/sh"})
			return err
		}, core.CodePermissionDenied},
		"executable not in allowlist": {func() error {
			_, err := f.svc.RunCommand(ctx, f.a, service.RunCommandRequest{OperationID: "x", Workspace: f.ws, Executable: "/bin/bash"})
			return err
		}, core.CodePermissionDenied},
		"timeout out of range": {func() error {
			_, err := f.svc.RunCommand(ctx, f.a, service.RunCommandRequest{OperationID: "x", Workspace: f.ws, Executable: "/bin/sh", TimeoutSeconds: 601})
			return err
		}, core.CodeInvalidRequest},
		"client without file.write": {func() error {
			_, err := f.svc.WriteFile(context.Background(), f.b, service.WriteFileRequest{OperationID: "x", Workspace: f.ws, Path: "f", Content: []byte("x")})
			return err
		}, core.CodePermissionDenied},
		"content too large": {func() error {
			_, err := f.svc.WriteFile(context.Background(), f.a, service.WriteFileRequest{OperationID: "x", Workspace: f.ws, Path: "f", Content: make([]byte, 8<<20+1)})
			return err
		}, core.CodeInvalidRequest},
		"client without file.read": {func() error {
			_, err := f.svc.ReadFile(context.Background(), f.c, service.ReadFileRequest{Workspace: f.ws, Path: "f"})
			return err
		}, core.CodePermissionDenied},
		"workspace Orca does not have": {func() error {
			_, err := f.svc.ListFiles(context.Background(), f.a, service.ListFilesRequest{Workspace: "nope"})
			return err
		}, core.CodeNotFound},
		"Orca project that is not a workspace": {func() error {
			f.fakes.Orca.AddRepo(core.Repo{ID: "git-1", DisplayName: "syntoria-reach", Path: "/src/syntoria-reach", Kind: "git"})
			_, err := f.svc.ListFiles(context.Background(), f.a, service.ListFilesRequest{Workspace: "git-1"})
			return err
		}, core.CodeNotFound},
		"path outside": {func() error {
			_, err := f.svc.ReadFile(context.Background(), f.a, service.ReadFileRequest{Workspace: f.ws, Path: "../x"})
			return err
		}, core.CodeInvalidRequest},
		"read limit": {func() error {
			_, err := f.svc.ReadFile(context.Background(), f.a, service.ReadFileRequest{Workspace: f.ws, Path: "f", Limit: 4<<20 + 1})
			return err
		}, core.CodeInvalidRequest},
	}
	for name, c := range cases {
		if err := c.call(); code(err) != c.code {
			t.Errorf("%s: %v", name, err)
		}
	}
	// Denied requests are not admitted as operations and run nothing.
	for _, client := range []string{"a", "b"} {
		if _, err := f.fakes.Store.GetOperation(client, "x"); !errors.Is(err, core.ErrNotFound) {
			t.Errorf("denied request of %s was admitted: %v", client, err)
		}
	}
	if len(f.fakes.Commands.Calls()) != 0 {
		t.Error("denied command ran")
	}
}

func TestFiles(t *testing.T) {
	f := newFixture(t)
	req := service.WriteFileRequest{OperationID: "w1", Workspace: f.ws, Path: "dir/f.txt", Content: []byte("one")}
	op, err := f.svc.WriteFile(context.Background(), f.a, req)
	if err != nil || op.State != core.StateSucceeded {
		t.Fatalf("op=%+v err=%v", op, err)
	}
	v1 := result[service.WriteFileResult](t, op).Version
	if b, _ := f.fakes.Files.Get(f.root, "dir/f.txt"); string(b) != "one" {
		t.Fatalf("content = %q", b)
	}

	// Creating again without a version fails, and the failure is stored.
	op, err = f.svc.WriteFile(context.Background(), f.a, service.WriteFileRequest{OperationID: "w2", Workspace: f.ws, Path: "dir/f.txt", Content: []byte("two")})
	if err != nil || op.State != core.StateFailed || op.ErrorCode != core.CodeVersionConflict {
		t.Fatalf("op=%+v err=%v", op, err)
	}
	op, err = f.svc.WriteFile(context.Background(), f.a, service.WriteFileRequest{OperationID: "w3", Workspace: f.ws, Path: "dir/f.txt", Content: []byte("two"), ExpectedVersion: v1})
	if err != nil || op.State != core.StateSucceeded {
		t.Fatalf("op=%+v err=%v", op, err)
	}

	chunk, err := f.svc.ReadFile(context.Background(), f.b, service.ReadFileRequest{Workspace: f.newWorkspace(t, f.b, "b"), Path: "x"})
	if code(err) != core.CodeNotFound || chunk != nil {
		t.Fatalf("missing file: %+v %v", chunk, err)
	}
	if chunk, err = f.svc.ReadFile(context.Background(), f.a, service.ReadFileRequest{Workspace: f.ws, Path: "dir/f.txt"}); err != nil || string(chunk.Data) != "two" || !chunk.EOF {
		t.Fatalf("read: %+v %v", chunk, err)
	}
	list, err := f.svc.ListFiles(context.Background(), f.a, service.ListFilesRequest{Workspace: f.ws})
	if err != nil || len(list.Entries) != 1 || list.Entries[0].Name != "dir" {
		t.Fatalf("list: %+v %v", list, err)
	}
	// One open root per workspace, released by Close.
	if n := f.fakes.Files.OpenRoots(f.root); n != 1 {
		t.Fatalf("open roots = %d", n)
	}
	f.svc.Close()
	if n := f.fakes.Files.OpenRoots(f.root); n != 0 {
		t.Fatalf("open roots after close = %d", n)
	}
}

func TestAuditHasNoContent(t *testing.T) {
	f := newFixture(t)
	f.svc.WriteFile(context.Background(), f.a, service.WriteFileRequest{OperationID: "w1", Workspace: f.ws, Path: "f.txt", Content: []byte("TOPSECRET")})
	f.svc.RunCommand(context.Background(), f.b, service.RunCommandRequest{OperationID: "c1", Workspace: f.ws, Executable: "/bin/sh", Args: []string{"SECRET-ARG"}})
	want := []string{"a|write_file|f.txt|ok|", "b|run_command|/bin/sh|denied|permission_denied"}
	if got := f.audit(); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("audit = %v", got)
	}
	b, _ := json.Marshal(f.fakes.Store.AuditRecords())
	if strings.Contains(string(b), "SECRET") {
		t.Fatalf("audit holds request content: %s", b)
	}
}
