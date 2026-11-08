package localapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/takezoh/syntoria-reach/src/internal/config"
	"github.com/takezoh/syntoria-reach/src/internal/core"
	"github.com/takezoh/syntoria-reach/src/internal/localws"
	"github.com/takezoh/syntoria-reach/src/internal/service"
	"github.com/takezoh/syntoria-reach/src/internal/service/servicetest"
)

const token = "token-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func start(t *testing.T) *http.Client {
	t.Helper()
	// Unix socket paths are limited to ~108 bytes, so avoid long temp dirs.
	dir, err := os.MkdirTemp("/tmp", "reach-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	// The service runs on fakes: this package owns HTTP decoding, peer and
	// token checks and error mapping, not the operations themselves.
	f := servicetest.New()
	svc := service.New(&config.Config{
		Workspaces: "/w",
		Clients:    []config.Client{{ID: "a", Token: token, Allow: []string{core.PermWorkspaceCreate, core.PermFileRead, core.PermFileWrite}}},
	}, f.Deps(localws.New("/w", f.Records, f.Folders, f.Agents, servicetest.NewIDs())))
	t.Cleanup(svc.Close)

	sock := filepath.Join(dir, "reach.sock")
	l, err := Listen(sock)
	if err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Stat(sock); info.Mode().Perm() != 0o600 {
		t.Fatalf("socket mode %v", info.Mode().Perm())
	}
	srv := NewServer(svc)
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sock)
		},
	}}
}

func post(t *testing.T, c *http.Client, method, tok string, body any) (int, map[string]any) {
	t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, "http://reach/v1/"+method, bytes.NewReader(b))
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func TestLocalAPI(t *testing.T) {
	c := start(t)
	status, created := post(t, c, "create_workspace", token, map[string]any{"operation_id": "cw", "name": "ws", "note": "localapi test"})
	if status != 200 || created["state"] != "succeeded" {
		t.Fatalf("create_workspace: %d %v", status, created)
	}
	ws := created["result"].(map[string]any)["workspace"]

	if status, out := post(t, c, "list_files", "", map[string]any{"workspace": ws}); status != 401 {
		t.Fatalf("no token: %d %v", status, out)
	}
	if status, _ := post(t, c, "list_files", "wrong-token", map[string]any{"workspace": ws}); status != 401 {
		t.Fatalf("wrong token: %d", status)
	}
	if status, _ := post(t, c, "read_file", token, map[string]any{"workspace": ws, "path": "x", "bogus": 1}); status != 400 {
		t.Fatalf("unknown field: %d", status)
	}

	status, op := post(t, c, "write_file", token, map[string]any{
		"operation_id": "w1", "workspace": ws, "path": "hello.txt", "content": []byte("hi"),
	})
	if status != 200 || op["state"] != "succeeded" {
		t.Fatalf("write: %d %v", status, op)
	}
	status, res := post(t, c, "read_file", token, map[string]any{"workspace": ws, "path": "hello.txt"})
	if status != 200 || res["data"] != "aGk=" || res["version"] != op["result"].(map[string]any)["version"] {
		t.Fatalf("read: %d %v", status, res)
	}
	status, got := post(t, c, "get_operation", token, map[string]any{"operation_id": "w1"})
	if status != 200 || got["state"] != "succeeded" {
		t.Fatalf("get_operation: %d %v", status, got)
	}
	if status, out := post(t, c, "run_command", token, map[string]any{"operation_id": "c1", "workspace": ws, "executable": "/bin/sh"}); status != 403 {
		t.Fatalf("run_command without permission: %d %v", status, out)
	}
	status, out := post(t, c, "write_file", token, map[string]any{"operation_id": "w1", "workspace": ws, "path": "other.txt"})
	if e, _ := out["error"].(map[string]any); status != 409 || e["code"] != core.CodeOperationConflict {
		t.Fatalf("operation conflict: %d %v", status, out)
	}
	if status, out := post(t, c, "no_such_method", token, map[string]any{}); status != 404 {
		t.Fatalf("unknown method: %d %v", status, out)
	}
	resp, err := c.Get("http://reach/v1/list_capabilities")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("GET: %d", resp.StatusCode)
	}
}
