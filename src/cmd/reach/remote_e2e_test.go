package main

// End-to-end test of the remote path: this test is the MCP client. It runs
// the gateway in workerd (wrangler dev) against a fake Syntoria Gate, which
// answers OIDC discovery and token introspection, and drives a workspace and
// an agent session on the real daemon, which reaches the gateway over its
// outbound WebSocket. It also covers a node going offline and coming back,
// and the gateway refusing tokens that Gate does not vouch for, tokens of
// another person, a connection revoked at Gate, and Gate being down.
//
// It needs Node.js and the gateway's packages (npm install in cloud/), so it
// runs only with REACH_E2E_GATEWAY=1.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/takezoh/syntoria-reach/src/internal/orca/orcatest"
)

const (
	gatewayNodeToken = "node-token-0123456789abcdef0123456789"
	gateClientID     = "reach-e2e"
	gateClientSecret = "gate-secret-0123456789abcdef0123456789"
	ownerSubject     = "github:1"
)

func TestRemoteE2E(t *testing.T) {
	if os.Getenv("REACH_E2E_GATEWAY") == "" {
		t.Skip("set REACH_E2E_GATEWAY=1 to run the gateway with wrangler dev (needs npm install in cloud/)")
	}
	gate := newFakeGate(t)
	base := startGateway(t, "GATE_ISSUER="+gate.URL, "GATE_CLIENT_ID="+gateClientID,
		"GATE_CLIENT_SECRET="+gateClientSecret, "OWNER_SUBJECT="+ownerSubject, "NODE_TOKEN="+gatewayNodeToken)
	resource := base + "/mcp"
	connectURL := "ws" + strings.TrimPrefix(base, "http") + "/connect"

	// The node token is required to connect.
	if _, resp, err := websocket.Dial(t.Context(), connectURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer wrong-token-0123456789abcdef012345"}},
	}); err == nil || resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong node token: resp=%v err=%v", resp, err)
	}

	// An MCP host without a token is sent to Gate through the resource
	// metadata.
	anonymous := &mcpClient{t: t, url: resource}
	if status, header := anonymous.status(); status != http.StatusUnauthorized ||
		header != `Bearer resource_metadata="`+base+`/.well-known/oauth-protected-resource/mcp"` {
		t.Fatalf("unauthenticated MCP request: %d %q", status, header)
	}
	var prm struct {
		Resource             string   `json:"resource"`
		AuthorizationServers []string `json:"authorization_servers"`
		ScopesSupported      []string `json:"scopes_supported"`
	}
	getJSON(t, base+"/.well-known/oauth-protected-resource/mcp", &prm)
	if prm.Resource != resource || !slices.Equal(prm.AuthorizationServers, []string{gate.URL}) || !slices.Contains(prm.ScopesSupported, "openid") {
		t.Fatalf("resource metadata: %+v", prm)
	}

	d, _ := newDaemon(t, fmt.Sprintf(`"remote": {"url": %q, "token": %q, "client": "e2e"},`, connectURL, gatewayNodeToken))
	d.start()

	// The token Gate issues to an approved MCP host of the owner.
	gate.issue("tok-owner", ownerSubject, resource)
	c := &mcpClient{t: t, url: resource, token: "tok-owner"}
	c.initialize()
	var names []string
	for _, tool := range c.rpc("tools/list", map[string]any{})["tools"].([]any) {
		names = append(names, tool.(map[string]any)["name"].(string))
	}
	for _, want := range []string{"list_capabilities", "list_workspaces", "create_workspace", "update_workspace", "start_agent_session", "send_agent_prompt", "read_agent_output", "stop_agent_session", "get_operation"} {
		if !slices.Contains(names, want) {
			t.Fatalf("tools %v lack %s", names, want)
		}
	}
	// The gateway asked Gate, with its resource client credentials.
	if gate.introspections() == 0 {
		t.Fatal("the gateway did not introspect the token")
	}

	// The daemon connects on its own; wait until requests reach it.
	caps := c.waitTool("list_capabilities", map[string]any{}, func(r any, e *toolError) bool { return e == nil }).(map[string]any)
	if agents, _ := caps["agents"].([]any); len(agents) != 1 || agents[0] != "claude" {
		t.Fatalf("list_capabilities: %v", caps)
	}

	// A workspace and an agent session, used through MCP.
	op := c.mustTool("create_workspace", map[string]any{"operation_id": "r-cw", "name": "ws", "note": "remote e2e"}).(map[string]any)
	if op["state"] != "succeeded" || result(op)["note"] != "remote e2e" {
		t.Fatalf("create_workspace: %v", op)
	}
	wsID := result(op)["workspace"]
	// Ongoing work is found again by its note and status.
	if op := c.mustTool("update_workspace", map[string]any{"operation_id": "r-uw", "workspace": wsID, "status": "in-review"}).(map[string]any); result(op)["status"] != "in-review" {
		t.Fatalf("update_workspace: %v", op)
	}
	if list := c.mustTool("list_workspaces", map[string]any{}).([]any); len(list) != 1 || list[0].(map[string]any)["note"] != "remote e2e" {
		t.Fatalf("list_workspaces: %v", list)
	}
	op = c.mustTool("start_agent_session", map[string]any{"operation_id": "r-s", "workspace": wsID, "agent": "claude"}).(map[string]any)
	sessionID := result(op)["session_id"]
	prompt := map[string]any{"operation_id": "r-p", "session_id": sessionID, "text": "hi"}
	if op := c.mustTool("send_agent_prompt", prompt).(map[string]any); op["state"] != "succeeded" {
		t.Fatalf("send_agent_prompt: %v", op)
	}
	if out := c.mustTool("read_agent_output", map[string]any{"session_id": sessionID}).(map[string]any); out["reply"] != "echo: hi" || out["state"] != "idle" {
		t.Fatalf("read_agent_output: %v", out)
	}
	// Resending the prompt with the same operation ID types nothing again.
	c.mustTool("send_agent_prompt", prompt)
	if n := strings.Count(orcatest.Calls(t, filepath.Join(filepath.Dir(d.sock), "orca")), "terminal send "); n != 1 {
		t.Fatalf("the prompt was typed %d times", n)
	}
	if s := c.mustTool("get_agent_session", map[string]any{"session_id": sessionID}).(map[string]any); s["workspace"] != wsID || s["state"] != "idle" {
		t.Fatalf("get_agent_session: %v", s)
	}
	// Errors from the node come back as tool errors with Reach's codes.
	if _, e := c.tool("get_agent_session", map[string]any{"session_id": "as_nope"}); e == nil || e.Code != "not_found" {
		t.Fatalf("unknown session: %v", e)
	}
	c.mustTool("stop_agent_session", map[string]any{"operation_id": "r-x", "session_id": sessionID})

	// The node goes offline: nothing runs, and the gateway says so. When
	// it comes back it reconnects by itself.
	d.kill()
	c.waitTool("list_capabilities", map[string]any{}, func(r any, e *toolError) bool { return e != nil && e.Code == "node_offline" })
	d.start()
	c.waitTool("list_capabilities", map[string]any{}, func(r any, e *toolError) bool { return e == nil })
	if op := c.mustTool("update_workspace", map[string]any{"operation_id": "r-done", "workspace": wsID, "status": "completed"}).(map[string]any); op["state"] != "succeeded" {
		t.Fatalf("complete the workspace: %v", op)
	}

	// Tokens Gate does not vouch for, or vouches for another resource, are
	// refused and send the host back to Gate.
	gate.issue("tok-elsewhere", ownerSubject, "https://other.example.test/mcp")
	for _, token := range []string{"not-a-token", "tok-elsewhere"} {
		if status, header := (&mcpClient{t: t, url: resource, token: token}).status(); status != http.StatusUnauthorized || !strings.Contains(header, "resource_metadata=") {
			t.Fatalf("token %s: %d %q", token, status, header)
		}
	}
	// A valid token of another person is not admitted.
	gate.issue("tok-stranger", "github:2", resource)
	if status, _ := (&mcpClient{t: t, url: resource, token: "tok-stranger"}).status(); status != http.StatusForbidden {
		t.Fatalf("another subject: %d", status)
	}
	// Gate being down is an outage, not a sign-out.
	gate.setDown(true)
	if status, _ := c.status(); status != http.StatusServiceUnavailable {
		t.Fatalf("Gate down: %d", status)
	}
	gate.setDown(false)
	// Revoking the connection at Gate stops it at the next request: nothing
	// is cached.
	if status, _ := c.status(); status != http.StatusOK {
		t.Fatalf("Gate back: %d", status)
	}
	gate.revoke("tok-owner")
	if status, _ := c.status(); status != http.StatusUnauthorized {
		t.Fatalf("revoked connection: %d", status)
	}
}

// fakeGate stands in for Syntoria Gate: OIDC discovery, and introspection of
// the tokens it was told about, for the gateway's resource client only.
type fakeGate struct {
	*httptest.Server
	mu     sync.Mutex
	tokens map[string]map[string]any
	down   bool
	calls  int
}

func newFakeGate(t *testing.T) *fakeGate {
	g := &fakeGate{tokens: map[string]map[string]any{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"issuer": g.URL, "introspection_endpoint": g.URL + "/token/introspection"})
	})
	mux.HandleFunc("POST /token/introspection", func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		if g.down {
			http.Error(w, "unavailable", http.StatusInternalServerError)
			return
		}
		if id, secret, ok := r.BasicAuth(); !ok || id != gateClientID || secret != gateClientSecret {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "invalid_client"})
			return
		}
		g.calls++
		res, ok := g.tokens[r.PostFormValue("token")]
		if !ok {
			res = map[string]any{"active": false}
		}
		writeJSON(w, http.StatusOK, res)
	})
	g.Server = httptest.NewServer(mux)
	t.Cleanup(g.Close)
	return g
}

func (g *fakeGate) issue(token, subject, audience string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.tokens[token] = map[string]any{
		"active": true, "token_type": "Bearer", "gate_approved": true, "iss": g.URL, "sub": subject, "aud": audience,
		"connection_id": "conn-" + token, "client_id": "client-" + token, "exp": time.Now().Add(time.Hour).Unix(),
	}
}

func (g *fakeGate) revoke(token string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.tokens[token] = map[string]any{"active": false}
}

func (g *fakeGate) setDown(down bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.down = down
}

func (g *fakeGate) introspections() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.calls
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// startGateway runs wrangler dev on a free port, with env added to its
// environment, and returns its base URL.
func startGateway(t *testing.T, env ...string) string {
	t.Helper()
	dir, err := filepath.Abs("../../../cloud")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "node_modules", ".bin", "wrangler")); err != nil {
		t.Fatalf("wrangler is not installed: run npm install in %s", dir)
	}
	port, inspector := freePort(t), freePort(t)
	state := t.TempDir()
	cmd := exec.Command(filepath.Join(dir, "node_modules", ".bin", "wrangler"), "dev",
		"--ip", "127.0.0.1", "--port", port, "--inspector-port", inspector,
		"--persist-to", filepath.Join(state, "wrangler"), "--log-level", "warn")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "WRANGLER_SEND_METRICS=false", "WRANGLER_LOG_PATH="+filepath.Join(state, "logs"), "CI=1")
	cmd.Env = append(cmd.Env, env...)
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		done := make(chan struct{})
		go func() { cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			<-done
		}
	})
	base := "http://127.0.0.1:" + port
	for range 600 {
		if resp, err := http.Get(base + "/"); err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return base
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("the gateway did not start")
	return ""
}

func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return fmt.Sprint(l.Addr().(*net.TCPAddr).Port)
}

func getJSON(t *testing.T, u string, v any) {
	t.Helper()
	resp, err := http.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: %d", u, resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		t.Fatalf("GET %s: %v", u, err)
	}
}

// status sends tools/list and returns the HTTP status and WWW-Authenticate.
func (c *mcpClient) status() (int, string) {
	c.t.Helper()
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"})
	req, _ := http.NewRequest(http.MethodPost, c.url, bytes.NewReader(b))
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if c.version != "" {
		req.Header.Set("MCP-Protocol-Version", c.version)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode, resp.Header.Get("WWW-Authenticate")
}

// mcpClient speaks MCP Streamable HTTP (JSON responses) with a bearer token.
type mcpClient struct {
	t       *testing.T
	url     string
	token   string
	version string
	id      int
}

type toolError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (c *mcpClient) post(msg map[string]any) (int, map[string]any) {
	c.t.Helper()
	b, _ := json.Marshal(msg)
	req, _ := http.NewRequest(http.MethodPost, c.url, bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if c.version != "" {
		req.Header.Set("MCP-Protocol-Version", c.version)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func (c *mcpClient) rpc(method string, params any) map[string]any {
	c.t.Helper()
	c.id++
	status, out := c.post(map[string]any{"jsonrpc": "2.0", "id": c.id, "method": method, "params": params})
	res, ok := out["result"].(map[string]any)
	if status != http.StatusOK || !ok {
		c.t.Fatalf("%s: %d %v", method, status, out)
	}
	return res
}

func (c *mcpClient) initialize() {
	c.t.Helper()
	res := c.rpc("initialize", map[string]any{
		"protocolVersion": "2025-11-25", "capabilities": map[string]any{},
		"clientInfo": map[string]any{"name": "reach-e2e", "version": "0"},
	})
	c.version, _ = res["protocolVersion"].(string)
	if c.version == "" || !strings.Contains(fmt.Sprint(res["instructions"]), "operation_id") {
		c.t.Fatalf("initialize: %v", res)
	}
	if status, _ := c.post(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"}); status != http.StatusAccepted {
		c.t.Fatalf("initialized notification: %d", status)
	}
}

// tool calls a tool and decodes its JSON text: the result, or the Reach
// error of a tool error.
func (c *mcpClient) tool(name string, args map[string]any) (any, *toolError) {
	c.t.Helper()
	res := c.rpc("tools/call", map[string]any{"name": name, "arguments": args})
	content, _ := res["content"].([]any)
	if len(content) != 1 {
		c.t.Fatalf("%s: content %v", name, res)
	}
	text := content[0].(map[string]any)["text"].(string)
	if res["isError"] == true {
		var e struct {
			Error *toolError `json:"error"`
		}
		if err := json.Unmarshal([]byte(text), &e); err != nil || e.Error == nil {
			c.t.Fatalf("%s: tool error %q", name, text)
		}
		return nil, e.Error
	}
	var v any
	if err := json.Unmarshal([]byte(text), &v); err != nil {
		c.t.Fatalf("%s: result %q", name, text)
	}
	return v, nil
}

func (c *mcpClient) mustTool(name string, args map[string]any) any {
	c.t.Helper()
	v, e := c.tool(name, args)
	if e != nil {
		c.t.Fatalf("%s: %+v", name, e)
	}
	return v
}

func (c *mcpClient) waitTool(name string, args map[string]any, cond func(any, *toolError) bool) any {
	c.t.Helper()
	var last any
	var lastErr *toolError
	for range 200 {
		last, lastErr = c.tool(name, args)
		if cond(last, lastErr) {
			return last
		}
		time.Sleep(50 * time.Millisecond)
	}
	c.t.Fatalf("%s %v: condition never held; last %v %+v", name, args, last, lastErr)
	return nil
}
