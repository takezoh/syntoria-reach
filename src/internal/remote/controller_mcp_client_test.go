package remote

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/takezoh/syntoria-reach/src/internal/config"
	"github.com/takezoh/syntoria-reach/src/internal/core"
	"github.com/takezoh/syntoria-reach/src/internal/localapi"
	"github.com/takezoh/syntoria-reach/src/internal/localws"
	"github.com/takezoh/syntoria-reach/src/internal/service"
	"github.com/takezoh/syntoria-reach/src/internal/service/servicetest"
)

func TestStdioConnectionInitializesAndReusesChild(t *testing.T) {
	// The fake speaks newline-delimited MCP and remains alive for both calls.
	script := `read init; printf '%s\n' '{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-03-26","capabilities":{},"serverInfo":{"name":"fake","version":"1"}}}'; read ready; read tools; printf '%s\n' '{"jsonrpc":"2.0","id":2,"result":{"tools":[{"name":"echo"}]}}'; read call; printf '%s\n' '{"jsonrpc":"2.0","id":3,"result":{"content":[{"type":"text","text":"ok"}]}}'; read end`
	c, err := Stdio(context.Background(), []string{"/bin/sh", "-c", script}, "/tmp/reach.sock", "token-sentinel")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	tools, err := c.Tools(context.Background())
	if err != nil || !strings.Contains(string(tools), `"echo"`) {
		t.Fatalf("tools=%s err=%v", tools, err)
	}
	result, err := c.CallTool(context.Background(), "echo", json.RawMessage(`{"x":1}`))
	if err != nil || !strings.Contains(string(result), `"ok"`) {
		t.Fatalf("result=%s err=%v", result, err)
	}
}

func TestStdioNotificationDoesNotPoisonPendingOrFollowingRequests(t *testing.T) {
	script := `read init; printf '%s\n' '{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-03-26"}}'; read ready; read tools; printf '%s\n' '{"jsonrpc":"2.0","method":"notifications/message","params":{"level":"info","data":"working"}}'; printf '%s\n' '{"jsonrpc":"2.0","id":2,"result":{"tools":[{"name":"echo"}]}}'; read call; printf '%s\n' '{"jsonrpc":"2.0","id":3,"result":{"content":[{"type":"text","text":"still-alive"}]}}'; read end`
	c, err := Stdio(context.Background(), []string{"/bin/sh", "-c", script}, "/tmp/reach.sock", "secret")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Tools(context.Background()); err != nil {
		t.Fatalf("valid notification interrupted tools/list: %v", err)
	}
	result, err := c.CallTool(context.Background(), "echo", json.RawMessage(`{}`))
	if err != nil || !strings.Contains(string(result), "still-alive") {
		t.Fatalf("connection did not handle request after notification: result=%s err=%v", result, err)
	}
}

func TestCanceledStdioCallDoesNotConsumeFollowingResponse(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, "first")
	second := filepath.Join(dir, "second")
	release := filepath.Join(dir, "release")
	script := fmt.Sprintf(`read init; printf '%%s\n' '{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-03-26"}}'; read ready; read first; : > %q; read second; : > %q; while [ ! -e %q ]; do sleep 0.01; done; printf '%%s\n' '{"jsonrpc":"2.0","id":3,"result":{"content":[{"type":"text","text":"second-response"}]}}'; printf '%%s\n' '{"jsonrpc":"2.0","id":2,"result":{"content":[{"type":"text","text":"late-first-response"}]}}'; read end`, first, second, release)
	c, err := Stdio(context.Background(), []string{"/bin/sh", "-c", script}, "/tmp/reach.sock", "secret")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx, cancel := context.WithCancel(context.Background())
	firstDone := make(chan error, 1)
	go func() { _, err := c.CallTool(ctx, "echo", json.RawMessage(`{}`)); firstDone <- err }()
	waitForFile(t, first)
	cancel()
	if err := <-firstDone; err == nil {
		t.Fatal("canceled call succeeded")
	}
	secondDone := make(chan struct {
		data json.RawMessage
		err  error
	}, 1)
	go func() {
		data, err := c.CallTool(context.Background(), "echo", json.RawMessage(`{}`))
		secondDone <- struct {
			data json.RawMessage
			err  error
		}{data, err}
	}()
	waitForFile(t, second)
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	got := <-secondDone
	if got.err != nil || !strings.Contains(string(got.data), "second-response") {
		t.Fatalf("second request received %s, err=%v", got.data, got.err)
	}
}

const bootstrapToken = "stdio-bootstrap-token-0123456789abcdef"

func TestStdioChildAuthenticatesAndCreatesWorkspaceThroughLocalAPI(t *testing.T) {
	t.Setenv("REACH_SOCKET", "")
	t.Setenv("REACH_TOKEN", "")
	t.Setenv("UNRELATED_SENTINEL", "must-not-be-inherited")
	socket := filepath.Join(t.TempDir(), "reach.sock")
	workspaces := filepath.Join(t.TempDir(), "workspaces")
	cfg := &config.Config{
		Socket: socket, Workspaces: workspaces,
		Clients: []config.Client{{ID: "bootstrap", Token: bootstrapToken, Allow: []string{core.PermWorkspaceCreate}}},
	}
	fakes := servicetest.New()
	backend := localws.New(workspaces, fakes.Records, fakes.Folders, fakes.Agents, servicetest.NewIDs())
	svc := service.New(cfg, fakes.Deps(backend))
	t.Cleanup(svc.Close)
	listener, err := localapi.Listen(socket)
	if err != nil {
		t.Fatal(err)
	}
	server := localapi.NewServer(svc)
	go server.Serve(listener)
	t.Cleanup(func() { _ = server.Close(); _ = listener.Close() })
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	child := []string{executable, "-test.run=^TestStdioBootstrapHelperProcess$"}
	client, err := Stdio(context.Background(), child, socket, bootstrapToken)
	if err != nil {
		t.Fatalf("stdio child bootstrap failed: %v", err)
	}
	defer client.Close()
	if _, err := client.Tools(context.Background()); err != nil {
		t.Fatal(err)
	}
	result, err := client.CallTool(context.Background(), "bootstrap_result", json.RawMessage(`{}`))
	if err != nil || !strings.Contains(string(result), "reach-stdio-bootstrap") {
		t.Fatalf("workspace was not created through the authenticated local API: %s, %v", result, err)
	}
}

// TestStdioBootstrapHelperProcess is selected by the Stdio child process. It
// proves the injected socket and bearer token reach a real local API request,
// which completes workspace creation before the MCP initialize response.
func TestStdioBootstrapHelperProcess(t *testing.T) {
	socket, token := os.Getenv("REACH_SOCKET"), os.Getenv("REACH_TOKEN")
	if socket == "" {
		return
	}
	if token != bootstrapToken || os.Getenv("UNRELATED_SENTINEL") != "" {
		os.Exit(41)
	}
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}
	client := &http.Client{Transport: transport, Timeout: 3 * time.Second}
	defer transport.CloseIdleConnections()
	call := func(method string, body io.Reader) (*http.Response, error) {
		req, err := http.NewRequest(http.MethodPost, "http://reach/v1/"+method, body)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		return client.Do(req)
	}
	resp, err := call("list_capabilities", strings.NewReader(`{}`))
	if err != nil {
		os.Exit(42)
	}
	var capabilities struct {
		Permissions []string `json:"permissions"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&capabilities)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !containsString(capabilities.Permissions, core.PermWorkspaceCreate) {
		os.Exit(43)
	}
	resp, err = call("create_workspace", strings.NewReader(`{"operation_id":"stdio-bootstrap","name":"stdio-bootstrap","note":"created by MCP child"}`))
	if err != nil {
		os.Exit(44)
	}
	var operation struct {
		State  string `json:"state"`
		Result struct {
			Name string `json:"name"`
		}
	}
	_ = json.NewDecoder(resp.Body).Decode(&operation)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || operation.State != "succeeded" || operation.Result.Name != "reach-stdio-bootstrap" {
		os.Exit(45)
	}
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		var request struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if json.Unmarshal(scanner.Bytes(), &request) != nil {
			os.Exit(46)
		}
		if len(request.ID) == 0 {
			continue
		}
		var result any
		switch request.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": "2025-03-26", "capabilities": map[string]any{}, "serverInfo": map[string]string{"name": "bootstrap-fake", "version": "1"}}
		case "tools/list":
			result = map[string]any{"tools": []any{}}
		case "tools/call":
			result = map[string]any{"content": []any{map[string]string{"type": "text", "text": operation.Result.Name}}}
		default:
			os.Exit(47)
		}
		response, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": result})
		fmt.Fprintln(os.Stdout, string(response))
	}
	os.Exit(0)
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", path)
}

func TestStdioInitializeFailureIsReturned(t *testing.T) {
	script := `read init; printf '%s\n' '{"jsonrpc":"2.0","id":1,"error":{"code":-1,"message":"initialize failed"}}'`
	_, err := Stdio(context.Background(), []string{"/bin/sh", "-c", script}, "/tmp/reach.sock", "secret")
	if err == nil || !strings.Contains(err.Error(), "initialize failed") {
		t.Fatalf("expected initialize failure, got %v", err)
	}
}

func TestChildExitAfterInitializeReachesCaller(t *testing.T) {
	script := `read init; printf '%s\n' '{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-03-26"}}'; read ready`
	c, err := Stdio(context.Background(), []string{"/bin/sh", "-c", script}, "/tmp/reach.sock", "secret")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Tools(context.Background()); err == nil {
		t.Fatal("child exit was reported as a successful tools/list")
	}
}

func TestCloseTerminatesStdioChild(t *testing.T) {
	script := `read init; printf '%s\n' '{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-03-26"}}'; while read line; do :; done`
	c, err := Stdio(context.Background(), []string{"/bin/sh", "-c", script}, "/tmp/reach.sock", "secret")
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Tools(context.Background()); err == nil {
		t.Fatal("closed child connection remained usable")
	}
}

func TestHTTPUnavailableIsReturned(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	if _, err := HTTP(context.Background(), url); err == nil {
		t.Fatal("unavailable MCP endpoint initialized")
	}
}

func TestHTTPStreamableConnectionKeepsSession(t *testing.T) {
	var count int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count++
		if r.Header.Get("Accept") == "" {
			t.Error("missing response Accept")
		}
		if count > 1 && r.Header.Get("Mcp-Session-Id") != "session-1" {
			t.Errorf("missing session header: %q", r.Header.Get("Mcp-Session-Id"))
		}
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
		}
		if count == 1 {
			w.Header().Set("Mcp-Session-Id", "session-1")
		}
		if len(req.ID) == 0 {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		var result string
		switch req.Method {
		case "initialize":
			result = `{"protocolVersion":"2025-03-26","capabilities":{},"serverInfo":{"name":"fake","version":"1"}}`
		case "tools/list":
			result = `{"tools":[{"name":"echo"}]}`
		case "tools/call":
			result = `{"content":[{"type":"text","text":"http-ok"}]}`
		default:
			t.Errorf("method %s", req.Method)
			result = `{}`
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":%s}`, req.ID, result)
	}))
	defer srv.Close()
	c, err := HTTP(context.Background(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Tools(context.Background()); err != nil {
		t.Fatal(err)
	}
	res, err := c.CallTool(context.Background(), "echo", json.RawMessage(`{}`))
	if err != nil || !strings.Contains(string(res), "http-ok") {
		t.Fatalf("%s %v", res, err)
	}
	if count != 4 {
		t.Fatalf("got %d HTTP requests, want initialize, notification and two calls", count)
	}
}
