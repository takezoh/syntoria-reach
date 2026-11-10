package main

// End-to-end test of the remote path in workerd with signed Access assertions
// and three nodes: a daemon on fake Orca, a daemon using local workspaces and
// an offline node. The fake issuer substitutes only Access signing keys; this
// does not test Cloudflare's edge or a hosted client's OAuth authorization.
// Enable explicitly with REACH_E2E_GATEWAY=1 (requires cloud dependencies).

import (
	"bytes"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
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
	accessAudience = "reach-mcp-test-audience"
	ownerSubject   = "owner-subject"
	ownerEmail     = "owner@example.com"
)

// The nodes of the gateway and their tokens.
var nodeTokens = map[string]string{
	"desk": "node-token-desk-0123456789abcdef0123456789",
	"lab":  "node-token-lab-0123456789abcdef01234567890",
	"away": "node-token-away-0123456789abcdef0123456789",
}

func TestRemoteE2E(t *testing.T) {
	if os.Getenv("REACH_E2E_GATEWAY") == "" {
		t.Skip("set REACH_E2E_GATEWAY=1 to run the gateway with wrangler dev (needs npm install in cloud/)")
	}
	access := newFakeAccess(t)
	nodes, _ := json.Marshal(nodeTokens)
	base := startGateway(t, "ACCESS_TEAM_DOMAIN="+access.URL, "ACCESS_AUD="+accessAudience,
		"ACCESS_OWNER_EMAIL="+ownerEmail, "NODES="+string(nodes))
	resource := base + "/mcp"
	connectURL := "ws" + strings.TrimPrefix(base, "http") + "/connect"

	// Only a node's token admits a daemon.
	if _, resp, err := websocket.Dial(t.Context(), connectURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer wrong-token-0123456789abcdef012345"}},
	}); err == nil || resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong node token: resp=%v err=%v", resp, err)
	}

	// Origin requests need a verified edge assertion. Access owns discovery;
	// the Worker must not continue advertising Gate or invent its own flow.
	anonymous := &mcpClient{t: t, url: resource}
	if status, _ := anonymous.status(); status != http.StatusUnauthorized {
		t.Fatalf("missing assertion: %d", status)
	}
	for _, path := range []string{"/.well-known/oauth-protected-resource", "/.well-known/oauth-protected-resource/mcp"} {
		resp, err := http.Get(base + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("origin discovery %s: %d", path, resp.StatusCode)
		}
	}

	remote := func(node string) string {
		return fmt.Sprintf(`"remote": {"url": %q, "token": %q, "client": "e2e"},`, connectURL, nodeTokens[node])
	}
	desk, _ := newDaemon(t, remote("desk"))
	desk.start()
	lab, _ := buildDaemon(t, true, remote("lab"))
	lab.start()

	// The Access assertion the edge forwards after authenticating the owner.
	c := &mcpClient{t: t, url: resource, token: access.issue(ownerSubject, accessAudience, time.Now().Add(time.Hour))}
	c.initialize()
	var names []string
	for _, tool := range c.rpc("tools/list", map[string]any{})["tools"].([]any) {
		names = append(names, tool.(map[string]any)["name"].(string))
	}
	for _, want := range []string{"list_nodes", "list_capabilities", "list_workspaces", "create_workspace", "update_workspace", "start_agent_session", "send_agent_prompt", "read_agent_output", "stop_agent_session", "get_operation"} {
		if !slices.Contains(names, want) {
			t.Fatalf("tools %v lack %s", names, want)
		}
	}

	// The daemons connect on their own. list_nodes shows every node, the
	// ones connected with the agents the caller may start there and their
	// roles.
	listed := c.waitTool("list_nodes", map[string]any{}, func(r any, e *toolError) bool {
		online := 0
		for _, n := range nodesOf(r) {
			if n["online"] == true {
				online++
			}
		}
		return e == nil && online == 2
	})
	byNode := map[string]map[string]any{}
	for _, n := range nodesOf(listed) {
		byNode[n["node"].(string)] = n
	}
	if len(byNode) != 3 || byNode["away"]["online"] != false || byNode["away"]["agents"] != nil {
		t.Fatalf("list_nodes: %v", listed)
	}
	for _, node := range []string{"desk", "lab"} {
		agents, _ := byNode[node]["agents"].([]any)
		if len(agents) != 1 || agents[0].(map[string]any)["id"] != "claude" || agents[0].(map[string]any)["role"] != "Writes and reviews code" {
			t.Fatalf("agents of %s: %v", node, byNode[node])
		}
	}

	// Every other tool needs the node; an unknown node runs nothing.
	if !c.invalid("list_workspaces", map[string]any{}) {
		t.Fatal("a tool without its node was accepted")
	}
	if _, e := c.tool("list_workspaces", map[string]any{"node": "nowhere"}); e == nil || e.Code != "not_found" {
		t.Fatalf("unknown node: %v", e)
	}
	if _, e := c.tool("list_workspaces", map[string]any{"node": "away"}); e == nil || e.Code != "node_offline" {
		t.Fatalf("node that never connected: %v", e)
	}

	// A workspace and an agent session on the Orca node, used through MCP.
	op := c.mustTool("create_workspace", map[string]any{"node": "desk", "operation_id": "r-cw", "name": "ws", "note": "remote e2e"}).(map[string]any)
	if op["state"] != "succeeded" || result(op)["note"] != "remote e2e" {
		t.Fatalf("create_workspace: %v", op)
	}
	wsID := result(op)["workspace"]
	// Ongoing work is found again by its note and status.
	if op := c.mustTool("update_workspace", map[string]any{"node": "desk", "operation_id": "r-uw", "workspace": wsID, "status": "in-review"}).(map[string]any); result(op)["status"] != "in-review" {
		t.Fatalf("update_workspace: %v", op)
	}
	if list := c.mustTool("list_workspaces", map[string]any{"node": "desk"}).([]any); len(list) != 1 || list[0].(map[string]any)["note"] != "remote e2e" {
		t.Fatalf("list_workspaces: %v", list)
	}
	op = c.mustTool("start_agent_session", map[string]any{"node": "desk", "operation_id": "r-s", "workspace": wsID, "agent": "claude"}).(map[string]any)
	sessionID := result(op)["session_id"]
	prompt := map[string]any{"node": "desk", "operation_id": "r-p", "session_id": sessionID, "text": "hi"}
	if op := c.mustTool("send_agent_prompt", prompt).(map[string]any); op["state"] != "succeeded" {
		t.Fatalf("send_agent_prompt: %v", op)
	}
	if out := c.mustTool("read_agent_output", map[string]any{"node": "desk", "session_id": sessionID}).(map[string]any); out["reply"] != "echo: hi" || out["state"] != "idle" {
		t.Fatalf("read_agent_output: %v", out)
	}
	// Resending the prompt with the same operation ID types nothing again.
	c.mustTool("send_agent_prompt", prompt)
	if n := strings.Count(orcatest.Calls(t, filepath.Join(filepath.Dir(desk.sock), "orca")), "terminal send "); n != 1 {
		t.Fatalf("the prompt was typed %d times", n)
	}
	if s := c.mustTool("get_agent_session", map[string]any{"node": "desk", "session_id": sessionID}).(map[string]any); s["workspace"] != wsID || s["state"] != "idle" {
		t.Fatalf("get_agent_session: %v", s)
	}
	// Errors from the node come back as tool errors with Reach's codes.
	if _, e := c.tool("get_agent_session", map[string]any{"node": "desk", "session_id": "as_nope"}); e == nil || e.Code != "not_found" {
		t.Fatalf("unknown session: %v", e)
	}
	c.mustTool("stop_agent_session", map[string]any{"node": "desk", "operation_id": "r-x", "session_id": sessionID})

	// The same on the node with local workspaces; workspaces and
	// operations belong to their node.
	op = c.mustTool("create_workspace", map[string]any{"node": "lab", "operation_id": "r-cw", "name": "ws", "note": "remote e2e on lab"}).(map[string]any)
	labWS := result(op)["workspace"]
	if op["state"] != "succeeded" || labWS == wsID {
		t.Fatalf("create_workspace on lab: %v", op)
	}
	if op := c.mustTool("update_workspace", map[string]any{"node": "lab", "operation_id": "r-uw2", "workspace": wsID, "note": "x"}).(map[string]any); op["error_code"] != "not_found" {
		t.Fatalf("update of another node's workspace: %v", op)
	}
	if op := c.mustTool("get_operation", map[string]any{"node": "lab", "operation_id": "r-cw"}).(map[string]any); result(op)["workspace"] != labWS {
		t.Fatalf("the operation r-cw of lab: %v", op)
	}
	op = c.mustTool("start_agent_session", map[string]any{"node": "lab", "operation_id": "r-s", "workspace": labWS, "agent": "claude"}).(map[string]any)
	labSession := result(op)["session_id"]
	if op := c.mustTool("send_agent_prompt", map[string]any{"node": "lab", "operation_id": "r-p", "session_id": labSession, "text": "hi"}).(map[string]any); op["state"] != "succeeded" {
		t.Fatalf("send_agent_prompt on lab: %v", op)
	}
	out := c.waitTool("read_agent_output", map[string]any{"node": "lab", "session_id": labSession}, func(r any, e *toolError) bool {
		return e == nil && r.(map[string]any)["state"] != "working"
	}).(map[string]any)
	if out["reply"] != "echo hi" || out["state"] != "idle" {
		t.Fatalf("read_agent_output on lab: %v", out)
	}

	// One node goes offline: nothing runs there, the gateway says so, and
	// the other node keeps working. When it comes back it reconnects by
	// itself.
	desk.kill()
	c.waitTool("list_capabilities", map[string]any{"node": "desk"}, func(r any, e *toolError) bool { return e != nil && e.Code == "node_offline" })
	if n := c.mustTool("list_nodes", map[string]any{}); !online(n, "lab") || online(n, "desk") {
		t.Fatalf("list_nodes with desk offline: %v", n)
	}
	if s := c.mustTool("get_agent_session", map[string]any{"node": "lab", "session_id": labSession}).(map[string]any); s["state"] != "idle" {
		t.Fatalf("lab while desk is offline: %v", s)
	}
	desk.start()
	c.waitTool("list_capabilities", map[string]any{"node": "desk"}, func(r any, e *toolError) bool { return e == nil })
	if op := c.mustTool("update_workspace", map[string]any{"node": "desk", "operation_id": "r-done", "workspace": wsID, "status": "completed"}).(map[string]any); op["state"] != "succeeded" {
		t.Fatalf("complete the workspace: %v", op)
	}
	c.mustTool("stop_agent_session", map[string]any{"node": "lab", "operation_id": "r-x", "session_id": labSession})

	// Signature, issuer, audience and expiry are checked at the Worker itself.
	for name, token := range map[string]string{
		"malformed":         "not-a-token",
		"wrong audience":    access.issue(ownerSubject, "other-app", time.Now().Add(time.Hour)),
		"expired":           access.issue(ownerSubject, accessAudience, time.Now().Add(-time.Minute)),
		"bad signature":     c.token[:len(c.token)-8] + "AAAAAAAA",
		"wrong issuer":      access.sign(map[string]any{"iss": "https://other.example.test", "sub": ownerSubject, "aud": []string{accessAudience}, "iat": time.Now().Unix(), "exp": time.Now().Add(time.Hour).Unix()}),
		"missing expiry":    access.sign(map[string]any{"iss": access.URL, "sub": ownerSubject, "aud": []string{accessAudience}, "iat": time.Now().Unix()}),
		"missing issued at": access.sign(map[string]any{"iss": access.URL, "sub": ownerSubject, "aud": []string{accessAudience}, "exp": time.Now().Add(time.Hour).Unix()}),
		"missing subject":   access.sign(map[string]any{"iss": access.URL, "aud": []string{accessAudience}, "iat": time.Now().Unix(), "exp": time.Now().Add(time.Hour).Unix()}),
		"not yet valid":     access.sign(map[string]any{"iss": access.URL, "sub": ownerSubject, "aud": []string{accessAudience}, "iat": time.Now().Unix(), "exp": time.Now().Add(time.Hour).Unix(), "nbf": time.Now().Add(time.Hour).Unix()}),
		"oversized":         strings.Repeat("a", 16385),
	} {
		if status, _ := (&mcpClient{t: t, url: resource, token: token}).status(); status != http.StatusUnauthorized {
			t.Fatalf("%s: %d", name, status)
		}
	}
	for _, subject := range []string{"another-person", ""} {
		if status, _ := (&mcpClient{t: t, url: resource, token: access.issue(subject, accessAudience, time.Now().Add(time.Hour))}).status(); status != http.StatusForbidden {
			t.Fatalf("subject %q: %d", subject, status)
		}
	}
	// A service assertion cannot impersonate the owner, even with the same app audience.
	for name, claims := range map[string]map[string]any{
		"missing email": {"iss": access.URL, "aud": []string{accessAudience}, "sub": ownerSubject, "iat": time.Now().Unix(), "exp": time.Now().Add(time.Hour).Unix()},
		"empty subject": {"iss": access.URL, "aud": []string{accessAudience}, "sub": "", "email": ownerEmail, "iat": time.Now().Unix(), "exp": time.Now().Add(time.Hour).Unix()},
	} {
		if status, _ := (&mcpClient{t: t, url: resource, token: access.sign(claims)}).status(); status != http.StatusForbidden {
			t.Fatalf("%s: %d", name, status)
		}
	}
	// A valid JWT in Authorization alone is not an Access assertion.
	if status, _ := (&mcpClient{t: t, url: resource, token: c.token, bearerOnly: true}).status(); status != http.StatusUnauthorized {
		t.Fatalf("bearer bypass: %d", status)
	}
	// Cached keys are permitted; an issuer outage does not invalidate them.
	access.setDown(true)
	if status, _ := c.status(); status != http.StatusOK {
		t.Fatalf("cached keys: %d", status)
	}
	// A fresh Worker without cached keys reports an outage, never a sign-out.
	cold := startGateway(t, "ACCESS_TEAM_DOMAIN="+access.URL, "ACCESS_AUD="+accessAudience, "ACCESS_OWNER_EMAIL="+ownerEmail, "NODES="+string(nodes))
	if status, _ := (&mcpClient{t: t, url: cold + "/mcp", token: c.token}).status(); status != http.StatusServiceUnavailable {
		t.Fatalf("key service down: %d", status)
	}
	access.setDown(false)
	if status, _ := (&mcpClient{t: t, url: cold + "/mcp", token: c.token}).status(); status != http.StatusOK {
		t.Fatalf("key service recovered: %d", status)
	}

}

// Bad operator configuration must never open the MCP endpoint. Even when
// MCP authentication is unconfigured, node authentication remains independent.
func TestRemoteInvalidConfig(t *testing.T) {
	if os.Getenv("REACH_E2E_GATEWAY") == "" {
		t.Skip("set REACH_E2E_GATEWAY=1")
	}
	nodes, _ := json.Marshal(nodeTokens)
	for _, tc := range []struct{ name, issuer, audience, owner, nodes string }{
		{"missing owner", "https://access.example.com", accessAudience, "", string(nodes)},
		{"invalid issuer", "https://access.example.com/unsafe", accessAudience, ownerEmail, string(nodes)},
		{"malformed nodes", "https://access.example.com", accessAudience, ownerEmail, "{}"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := startGateway(t, "ACCESS_TEAM_DOMAIN="+tc.issuer, "ACCESS_AUD="+tc.audience, "ACCESS_OWNER_EMAIL="+tc.owner, "NODES="+tc.nodes)
			if status, _ := (&mcpClient{t: t, url: base + "/mcp"}).status(); status != http.StatusServiceUnavailable {
				t.Fatalf("bad configuration: %d", status)
			}
			if tc.nodes == string(nodes) {
				ws, resp, err := websocket.Dial(t.Context(), "ws"+strings.TrimPrefix(base, "http")+"/connect", &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + nodeTokens["desk"]}}})
				if err != nil {
					t.Fatalf("node channel depends on MCP configuration: %v %v", resp, err)
				}
				ws.CloseNow()
			}
		})
	}
}

// nodesOf returns the nodes of a list_nodes result.
func nodesOf(r any) []map[string]any {
	m, _ := r.(map[string]any)
	list, _ := m["nodes"].([]any)
	var out []map[string]any
	for _, n := range list {
		if n, ok := n.(map[string]any); ok {
			out = append(out, n)
		}
	}
	return out
}

// online reports whether a list_nodes result shows node online.
func online(r any, node string) bool {
	for _, n := range nodesOf(r) {
		if n["node"] == node {
			return n["online"] == true
		}
	}
	return false
}

// fakeAccess serves real RSA public keys and signs synthetic application JWTs.
// It does not emulate Managed OAuth or Access policy evaluation.
type fakeAccess struct {
	*httptest.Server
	key  *rsa.PrivateKey
	mu   sync.Mutex
	down bool
}

func newFakeAccess(t *testing.T) *fakeAccess {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	a := &fakeAccess{key: key}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /cdn-cgi/access/certs", func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		down := a.down
		a.mu.Unlock()
		if down {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"keys": []any{map[string]any{
			"kty": "RSA", "alg": "RS256", "use": "sig", "kid": "test-key",
			"n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
		}}})
	})
	a.Server = httptest.NewServer(mux)
	t.Cleanup(a.Close)
	return a
}
func (a *fakeAccess) sign(claims map[string]any) string {
	header, _ := json.Marshal(map[string]string{"alg": "RS256", "kid": "test-key"})
	payload, _ := json.Marshal(claims)
	input := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload)
	hash := sha256.Sum256([]byte(input))
	sig, err := rsa.SignPKCS1v15(rand.Reader, a.key, crypto.SHA256, hash[:])
	if err != nil {
		panic(err)
	}
	return input + "." + base64.RawURLEncoding.EncodeToString(sig)
}
func (a *fakeAccess) issue(subject, audience string, expiry time.Time) string {
	email := "other@example.com"
	if subject == ownerSubject {
		email = ownerEmail
	}
	return a.sign(map[string]any{"email": email, "iss": a.URL, "sub": subject, "aud": []string{audience}, "iat": time.Now().Unix(), "exp": expiry.Unix(), "type": "app"})
}
func (a *fakeAccess) setDown(down bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.down = down
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

// status sends tools/list and returns the HTTP status and WWW-Authenticate.
func (c *mcpClient) status() (int, string) {
	c.t.Helper()
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"})
	req, _ := http.NewRequest(http.MethodPost, c.url, bytes.NewReader(b))
	if c.token != "" {
		if c.bearerOnly {
			req.Header.Set("Authorization", "Bearer "+c.token)
		} else {
			req.Header.Set("Cf-Access-Jwt-Assertion", c.token)
		}
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

// mcpClient speaks MCP Streamable HTTP as requests forwarded by the Access edge.
type mcpClient struct {
	t          *testing.T
	url        string
	token      string
	bearerOnly bool
	version    string
	id         int
}

type toolError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (c *mcpClient) post(msg map[string]any) (int, map[string]any) {
	c.t.Helper()
	b, _ := json.Marshal(msg)
	req, _ := http.NewRequest(http.MethodPost, c.url, bytes.NewReader(b))
	if c.bearerOnly {
		req.Header.Set("Authorization", "Bearer "+c.token)
	} else {
		req.Header.Set("Cf-Access-Jwt-Assertion", c.token)
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

// invalid reports whether the gateway refused the call's arguments, as an
// MCP error or a tool error that is not one of Reach's, without running it.
func (c *mcpClient) invalid(name string, args map[string]any) bool {
	c.t.Helper()
	c.id++
	_, out := c.post(map[string]any{"jsonrpc": "2.0", "id": c.id, "method": "tools/call", "params": map[string]any{"name": name, "arguments": args}})
	if out["error"] != nil {
		return true
	}
	res, _ := out["result"].(map[string]any)
	content, _ := res["content"].([]any)
	if res["isError"] != true || len(content) != 1 {
		return false
	}
	text, _ := content[0].(map[string]any)["text"].(string)
	return !strings.Contains(text, `"error"`) && strings.Contains(text, "node")
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
