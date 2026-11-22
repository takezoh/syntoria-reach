package main

// End-to-end test of the remote path in workerd with signed Access assertions
// and three nodes: a daemon on fake Orca, a daemon using local workspaces and
// an offline node. The fake issuer substitutes only Access signing keys; this
// does not test Cloudflare's edge or a hosted client's OAuth authorization.
// Enable explicitly with REACH_E2E_GATEWAY=1 (requires cloud dependencies).

import (
	"bufio"
	"bytes"
	"context"
	"crypto"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/takezoh/syntoria-reach/src/internal/core"
	"github.com/takezoh/syntoria-reach/src/internal/orca/orcatest"
	"github.com/takezoh/syntoria-reach/src/internal/store"
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
	receiver := newWebhookReceiver(t)
	nodes, _ := json.Marshal(nodeTokens)
	gateway := newGateway(t, "WEBHOOK_ALLOWED_ORIGINS="+receiver.server.URL, "ACCESS_TEAM_DOMAIN="+access.URL, "ACCESS_AUD="+accessAudience,
		"ACCESS_OWNER_EMAIL="+ownerEmail, "NODES="+string(nodes))
	base := gateway.base
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
	c.eventsE2E(desk, sessionID.(string), wsID, names)
	prompt := map[string]any{"node": "desk", "operation_id": "r-p", "session_id": sessionID, "text": "hi"}
	if op := c.mustTool("send_agent_prompt", prompt).(map[string]any); op["state"] != "succeeded" {
		t.Fatalf("send_agent_prompt: %v", op)
	}
	if out := c.mustTool("read_agent_output", map[string]any{"node": "desk", "session_id": sessionID}).(map[string]any); out["reply"] != "echo: hi" || out["state"] != "idle" {
		t.Fatalf("read_agent_output: %v", out)
	}
	// Resending the prompt with the same operation ID types nothing again.
	c.mustTool("send_agent_prompt", prompt)
	if n := strings.Count(orcatest.Calls(t, filepath.Join(filepath.Dir(desk.sock), "orca")), "terminal send "); n != 5 {
		t.Fatalf("the prompt was typed %d times", n)
	}
	c.webhookE2E(desk, wsID, receiver, gateway)
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
		if status, _ := (&mcpClient{t: t, url: resource, token: access.issue(subject, accessAudience, time.Now().Add(time.Hour))}).post(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "events/poll", "params": map[string]any{}}); status != http.StatusForbidden {
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
func startGateway(t *testing.T, env ...string) string { return newGateway(t, env...).base }

type testGateway struct {
	t                                         *testing.T
	dir, config, port, inspector, state, base string
	env                                       []string
	cmd                                       *exec.Cmd
}

func newGateway(t *testing.T, env ...string) *testGateway {
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
	config := filepath.Join(dir, "wrangler.jsonc")
	for _, value := range env {
		if strings.HasPrefix(value, "WEBHOOK_ALLOWED_ORIGINS=http://127.0.0.1:") {
			data, err := os.ReadFile(config)
			if err != nil {
				t.Fatal(err)
			}
			configDir := t.TempDir()
			if err := os.Symlink(filepath.Join(dir, "src"), filepath.Join(configDir, "src")); err != nil {
				t.Fatal(err)
			}
			config = filepath.Join(configDir, "wrangler.jsonc")
			flags := []byte(`"nodejs_compat", "global_fetch_strictly_public"`)
			if !bytes.Contains(data, flags) {
				t.Fatalf("%s no longer lists the expected compatibility flags", config)
			}
			data = bytes.Replace(data, flags, []byte(`"nodejs_compat"`), 1)
			if err := os.WriteFile(config, data, 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	g := &testGateway{t: t, dir: dir, config: config, port: port, inspector: inspector, state: state, base: "http://127.0.0.1:" + port, env: env}
	g.start()
	t.Cleanup(g.stop)
	return g
}
func (g *testGateway) start() {
	t := g.t
	t.Helper()
	cmd := exec.Command(filepath.Join(g.dir, "node_modules", ".bin", "wrangler"), "dev", "--config", g.config,
		"--ip", "127.0.0.1", "--port", g.port, "--inspector-port", g.inspector,
		"--persist-to", filepath.Join(g.state, "wrangler"), "--log-level", "warn")
	for _, value := range g.env {
		if strings.HasPrefix(value, "WEBHOOK_ALLOWED_ORIGINS=") {
			cmd.Args = append(cmd.Args, "--var", strings.Replace(value, "=", ":", 1))
		}
	}
	cmd.Dir = g.dir
	cmd.Env = append(os.Environ(), "WRANGLER_SEND_METRICS=false", "WRANGLER_LOG_PATH="+filepath.Join(g.state, "logs"), "CI=1")
	cmd.Env = append(cmd.Env, g.env...)
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	g.cmd = cmd
	for range 600 {
		if resp, err := http.Get(g.base + "/"); err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("the gateway did not start")
}
func (g *testGateway) stop() {
	if g.cmd == nil {
		return
	}
	cmd := g.cmd
	g.cmd = nil
	syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	done := make(chan struct{})
	go func() { cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-done
	}
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
	if c.name != "" {
		req.Header.Set("Mcp-Name", c.name)
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
	name       string
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
	if c.token == "" {
		// An empty token sends no assertion header at all.
	} else if c.bearerOnly {
		req.Header.Set("Authorization", "Bearer "+c.token)
	} else {
		req.Header.Set("Cf-Access-Jwt-Assertion", c.token)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if c.version != "" {
		req.Header.Set("MCP-Protocol-Version", c.version)
	}
	if c.name != "" {
		req.Header.Set("Mcp-Name", c.name)
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

const eventName = "agent.session_state_changed"

func (c *mcpClient) eventsE2E(d *daemon, session string, workspace any, oldNames []string) {
	t := c.t
	modern := &mcpClient{t: t, url: c.url, token: c.token, version: "2026-07-28", name: eventName}
	chat := &mcpClient{t: t, url: c.url + "?profile=chatgpt", token: c.token, version: "2026-07-28"}
	for _, client := range []*mcpClient{modern, chat} {
		discover := client.rpc("server/discover", map[string]any{})
		caps := discover["capabilities"].(map[string]any)
		if client == chat {
			if caps["events"] == nil {
				t.Fatalf("chat discovery: %v", discover)
			}
		} else if caps["extensions"].(map[string]any)["io.modelcontextprotocol/events"] == nil {
			t.Fatalf("standard discovery: %v", discover)
		}
		list := client.rpc("events/list", map[string]any{})["events"].([]any)
		if len(list) != 1 || list[0].(map[string]any)["name"] != eventName {
			t.Fatalf("events list: %v", list)
		}
		var names []string
		for _, tool := range client.rpc("tools/list", map[string]any{})["tools"].([]any) {
			names = append(names, tool.(map[string]any)["name"].(string))
		}
		if !reflect.DeepEqual(names, oldNames) {
			t.Fatalf("catalog: %v vs %v", names, oldNames)
		}
		client.mustTool("list_nodes", map[string]any{})
	}
	legacy := &mcpClient{t: t, url: chat.url, token: c.token}
	init := legacy.rpc("initialize", map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "reach-e2e", "version": "0"}})
	if init["capabilities"].(map[string]any)["events"] == nil {
		t.Fatalf("legacy chat capabilities: %v", init)
	}
	legacy.version = init["protocolVersion"].(string)
	var legacyNames []string
	for _, tool := range legacy.rpc("tools/list", map[string]any{})["tools"].([]any) {
		legacyNames = append(legacyNames, tool.(map[string]any)["name"].(string))
	}
	if !reflect.DeepEqual(legacyNames, oldNames) {
		t.Fatalf("legacy chat catalog: %v", legacyNames)
	}
	for _, method := range []string{"events/poll", "events/stream"} {
		_, out := chat.post(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": map[string]any{}})
		if out["error"].(map[string]any)["code"] != float64(-32601) {
			t.Fatalf("%s: %v", method, out)
		}
	}
	for _, token := range []string{"", "malformed"} {
		bad := *modern
		bad.token = token
		status, _ := bad.post(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "events/poll", "params": map[string]any{}})
		if status != http.StatusUnauthorized && status != http.StatusForbidden {
			t.Fatalf("event authentication: %d", status)
		}
	}
	args := map[string]any{"node": "desk", "session_id": session}
	params := map[string]any{"name": eventName, "arguments": args, "cursor": nil}
	poll := func(cursor any) map[string]any { params["cursor"] = cursor; return modern.rpc("events/poll", params) }
	initial := poll(nil)
	cursor := initial["cursor"]
	second := result(c.mustTool("start_agent_session", map[string]any{"node": "desk", "operation_id": "events-second", "workspace": workspace, "agent": "claude"}).(map[string]any))["session_id"]
	other := modern.rpc("events/poll", map[string]any{"name": eventName, "arguments": map[string]any{"node": "desk", "session_id": second}, "cursor": cursor})
	if other["truncated"] != true {
		t.Fatalf("cross-session cursor: %v", other)
	}
	c.mustTool("stop_agent_session", map[string]any{"node": "desk", "operation_id": "events-second-stop", "session_id": second})
	for _, name := range []string{"", "wrong"} {
		bad := *modern
		bad.name = name
		status, out := bad.post(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "events/poll", "params": params})
		if status != 400 || out["error"].(map[string]any)["code"] != float64(-32020) {
			t.Fatalf("header: %d %v", status, out)
		}
	}
	_, out := modern.post(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "events/subscribe", "params": map[string]any{"name": eventName, "arguments": args, "delivery": map[string]any{"mode": "push", "url": "https://example.test"}}})
	if out["error"].(map[string]any)["code"] != float64(-32026) {
		t.Fatalf("mode: %v", out)
	}
	_, out = modern.post(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "events/poll", "params": map[string]any{"name": eventName, "arguments": map[string]any{"node": "missing", "session_id": session}, "cursor": nil}})
	if out["error"] == nil {
		t.Fatalf("unknown node: %v", out)
	}
	c.mustTool("send_agent_prompt", map[string]any{"node": "desk", "operation_id": "events-poll", "session_id": session, "text": "events one"})
	var got map[string]any
	for range 100 {
		raw := json.RawMessage(fmt.Sprintf(`{"session_id":%q,"node":"desk"}`, session))
		got = modern.rpc("events/poll", map[string]any{"name": eventName, "arguments": raw, "cursor": cursor})
		if got["truncated"] != false {
			t.Fatalf("reordered cursor: %v", got)
		}
		if len(got["events"].([]any)) > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	entries := got["events"].([]any)
	if len(entries) != 1 {
		t.Fatalf("poll: %v", got)
	}
	data := entries[0].(map[string]any)["data"].(map[string]any)
	if len(data) != 4 || data["node"] != "desk" || data["session_id"] != session || data["state"] != "idle" || data["observed_at"] == nil {
		t.Fatalf("payload: %v", data)
	}
	if out := c.mustTool("read_agent_output", map[string]any{"node": "desk", "session_id": session}).(map[string]any); out["reply"] != "echo: events one" {
		t.Fatalf("output: %v", out)
	}
	cursor = got["cursor"]
	if again := poll(cursor); len(again["events"].([]any)) != 0 {
		t.Fatalf("duplicate: %v", again)
	}
	stream, cancel := modern.openEventStream(args, nil)
	active := readEvent(t, stream, "active")
	oldCursor := active["cursor"]
	c.mustTool("send_agent_prompt", map[string]any{"node": "desk", "operation_id": "events-push", "session_id": session, "text": "events two"})
	event := readEvent(t, stream, "event")
	if event["data"].(map[string]any)["session_id"] != session {
		t.Fatalf("stream: %v", event)
	}
	cancel()
	stream.Close()
	// Workerd detects an idle disconnect on a subsequent heartbeat write.
	time.Sleep(3500 * time.Millisecond)
	if out := c.mustTool("read_agent_output", map[string]any{"node": "desk", "session_id": session}).(map[string]any); out["reply"] != "echo: events two" {
		t.Fatalf("stream output: %v", out)
	}
	stream, cancel = modern.openEventStream(args, oldCursor)
	active = readEvent(t, stream, "active")
	if active["truncated"] != true {
		t.Fatalf("reopened: %v", active)
	}
	cancel()
	stream.Close()
	// Hold the fake backend while the prompt is in flight, then disconnect the reader.
	stream, cancel = modern.openEventStream(args, nil)
	readEvent(t, stream, "active")
	lock, err := os.OpenFile(filepath.Join(filepath.Dir(d.sock), "orca", "state.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	before := orcatest.Calls(t, filepath.Join(filepath.Dir(d.sock), "orca"))
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.mustTool("send_agent_prompt", map[string]any{"node": "desk", "operation_id": "events-abort", "session_id": session, "text": "after abort"})
	}()
	blocked := false
	for range 100 {
		if orcatest.Calls(t, filepath.Join(filepath.Dir(d.sock), "orca")) != before {
			blocked = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	stream.Close()
	// Keep the backend blocked until both the Worker (on a later heartbeat
	// write) and the broker's reader cleanup in the Durable Object have seen
	// the disconnect, before the prompt is allowed to continue.
	time.Sleep(5 * time.Second)
	finished := false
	select {
	case <-done:
		finished = true
	default:
	}
	syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	lock.Close()
	if finished {
		t.Fatal("prompt finished while backend was blocked")
	}
	if !blocked {
		t.Fatal("prompt did not reach blocked backend")
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("prompt did not continue after stream abort")
	}
	if out := c.mustTool("read_agent_output", map[string]any{"node": "desk", "session_id": session}).(map[string]any); out["reply"] != "echo: after abort" {
		t.Fatalf("abort output: %v", out)
	}
	// An unarmed pending turn is durable evidence of a crash before observation.
	cursor = poll(cursor)["cursor"]
	d.kill()
	db, err := store.Open(filepath.Join(filepath.Dir(d.sock), "reach.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err = db.BeginTurn(core.Turn{ClientID: "e2e", OperationID: "events-unknown", SessionID: session}); err != nil {
		t.Fatal(err)
	}
	db.Close()
	d.start()
	c.waitTool("list_capabilities", map[string]any{"node": "desk"}, func(_ any, e *toolError) bool { return e == nil })
	for range 100 {
		got = poll(cursor)
		if len(got["events"].([]any)) > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	entries = got["events"].([]any)
	if len(entries) != 1 || entries[0].(map[string]any)["data"].(map[string]any)["state"] != "unknown" {
		t.Fatalf("unknown: %v", got)
	}
	c.mustTool("read_agent_output", map[string]any{"node": "desk", "session_id": session})
	cursor = got["cursor"]
	d.kill()
	d.start()
	c.waitTool("list_capabilities", map[string]any{"node": "desk"}, func(_ any, e *toolError) bool { return e == nil })
	// The node relays its outbox in order, so a repeated unknown would arrive
	// before the stop of a prompt sent after this restart.
	c.mustTool("send_agent_prompt", map[string]any{"node": "desk", "operation_id": "events-after-restart", "session_id": session, "text": "after restart"})
	var seen []any
	for range 100 {
		got = poll(cursor)
		cursor = got["cursor"]
		seen = append(seen, got["events"].([]any)...)
		if len(seen) > 0 && seen[len(seen)-1].(map[string]any)["data"].(map[string]any)["state"] == "idle" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if len(seen) != 1 || seen[0].(map[string]any)["data"].(map[string]any)["state"] != "idle" {
		t.Fatalf("after the second restart: %v", seen)
	}
	if out := c.mustTool("read_agent_output", map[string]any{"node": "desk", "session_id": session}).(map[string]any); out["reply"] != "echo: after restart" {
		t.Fatalf("restart output: %v", out)
	}
}

// eventStream keeps one buffered reader for the life of the response, so a
// frame read ahead while finding one event is not lost for the next.
type eventStream struct {
	body   io.ReadCloser
	reader *bufio.Reader
}

func (s *eventStream) Close() error { return s.body.Close() }

func (c *mcpClient) openEventStream(args map[string]any, cursor any) (*eventStream, context.CancelFunc) {
	c.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 42, "method": "events/stream", "params": map[string]any{"name": eventName, "arguments": args, "cursor": cursor, "heartbeatMs": 1000}})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(b))
	req.Header.Set("Cf-Access-Jwt-Assertion", c.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("MCP-Protocol-Version", c.version)
	req.Header.Set("Mcp-Name", eventName)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		c.t.Fatalf("stream status: %s", resp.Status)
	}
	return &eventStream{body: resp.Body, reader: bufio.NewReader(resp.Body)}, cancel
}

func readEvent(t *testing.T, stream *eventStream, method string) map[string]any {
	t.Helper()
	for {
		line, err := stream.reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var msg map[string]any
		if err = json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &msg); err != nil {
			t.Fatal(err)
		}
		if msg["method"] == "notifications/events/"+method {
			p := msg["params"].(map[string]any)
			if p["_meta"].(map[string]any)["io.modelcontextprotocol/subscriptionId"] != float64(42) {
				t.Fatalf("correlation: %v", msg)
			}
			return p
		}
	}
}

// webhookReceiver independently implements the receiver side of the wire protocol.
type webhookMessage struct {
	id, timestamp, body, subscription string
	accepted                          bool
}
type webhookReceiver struct {
	t             *testing.T
	server        *httptest.Server
	mu            sync.Mutex
	secrets       map[string][]byte
	subscriptions map[string][]byte
	messages      map[string][]webhookMessage
	controls      map[string][]webhookMessage
	failChallenge map[string]bool
	failDelivery  map[string]bool
}

func newWebhookReceiver(t *testing.T) *webhookReceiver {
	r := &webhookReceiver{t: t, secrets: map[string][]byte{}, subscriptions: map[string][]byte{}, messages: map[string][]webhookMessage{}, controls: map[string][]webhookMessage{}, failChallenge: map[string]bool{}, failDelivery: map[string]bool{}}
	r.server = httptest.NewServer(http.HandlerFunc(r.handle))
	t.Cleanup(r.server.Close)
	return r
}
func (r *webhookReceiver) secret(path string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		r.t.Fatal(err)
	}
	r.secrets[path] = key
	return "whsec_" + base64.StdEncoding.EncodeToString(key)
}
func (r *webhookReceiver) handle(w http.ResponseWriter, req *http.Request) {
	body, err := io.ReadAll(req.Body)
	if err != nil {
		http.Error(w, "body", 400)
		return
	}
	var value map[string]any
	if json.Unmarshal(body, &value) != nil {
		http.Error(w, "json", 400)
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	id, stamp, sub := req.Header.Get("webhook-id"), req.Header.Get("webhook-timestamp"), req.Header.Get("X-MCP-Subscription-Id")
	key := r.subscriptions[sub]
	if value["type"] == "verification" {
		key = r.secrets[req.URL.Path]
	}
	mac := hmac.New(sha256.New, key)
	fmt.Fprintf(mac, "%s.%s.%s", id, stamp, body)
	expected := "v1," + base64.StdEncoding.EncodeToString(mac.Sum(nil))
	if len(key) == 0 || id == "" || stamp == "" || sub == "" || !hmac.Equal([]byte(expected), []byte(req.Header.Get("webhook-signature"))) {
		http.Error(w, "signature", 401)
		return
	}
	msg := webhookMessage{id: id, timestamp: stamp, body: string(body), subscription: sub, accepted: true}
	if _, control := value["type"]; control {
		r.controls[req.URL.Path] = append(r.controls[req.URL.Path], msg)
		if value["type"] == "verification" {
			if r.failChallenge[req.URL.Path] {
				delete(r.failChallenge, req.URL.Path)
				http.Error(w, "challenge", 500)
				return
			}
			r.subscriptions[sub] = key
			json.NewEncoder(w).Encode(map[string]any{"challenge": value["challenge"]})
			return
		}
		return
	}
	if r.failDelivery[req.URL.Path] {
		delete(r.failDelivery, req.URL.Path)
		msg.accepted = false
		r.messages[req.URL.Path] = append(r.messages[req.URL.Path], msg)
		http.Error(w, "retry", 500)
		return
	}
	r.messages[req.URL.Path] = append(r.messages[req.URL.Path], msg)
}
func (r *webhookReceiver) snapshot(path string) ([]webhookMessage, []webhookMessage) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.messages[path]), slices.Clone(r.controls[path])
}
func (r *webhookReceiver) wait(path string, n int) []webhookMessage {
	r.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		m, _ := r.snapshot(path)
		if len(m) >= n {
			return m
		}
		time.Sleep(50 * time.Millisecond)
	}
	m, _ := r.snapshot(path)
	r.t.Fatalf("webhook %s: got %d messages, want %d", path, len(m), n)
	return nil
}
func (c *mcpClient) webhookE2E(d *daemon, workspace any, r *webhookReceiver, gateway *testGateway) {
	t := c.t
	session := result(c.mustTool("start_agent_session", map[string]any{"node": "desk", "operation_id": "webhook-session", "workspace": workspace, "agent": "claude"}).(map[string]any))["session_id"].(string)
	standard := &mcpClient{t: t, url: c.url, token: c.token, version: "2026-07-28", name: eventName}
	chat := &mcpClient{t: t, url: c.url + "?profile=chatgpt", token: c.token, version: "2026-07-28"}
	if chat.rpc("server/discover", map[string]any{})["capabilities"].(map[string]any)["events"] == nil {
		t.Fatal("missing chat events")
	}
	chat.rpc("tools/list", map[string]any{})
	params := func(path, secret string) map[string]any {
		return map[string]any{"name": eventName, "arguments": map[string]any{"node": "desk", "session_id": session}, "delivery": map[string]any{"mode": "webhook", "url": r.server.URL + path, "secret": secret}}
	}
	a, b, ch := params("/a", r.secret("/a")), params("/b", r.secret("/b")), params("/chat", r.secret("/chat"))
	first := standard.rpc("events/subscribe", a)
	expiry := params("/expiry", r.secret("/expiry"))
	expiry["ttlMs"] = 60000
	grant := standard.rpc("events/subscribe", expiry)
	deadline, err := time.Parse(time.RFC3339Nano, grant["refreshBefore"].(string))
	if err != nil {
		t.Fatal(err)
	}
	standard.rpc("events/subscribe", b)
	sub := chat.rpc("events/subscribe", ch)
	ch["arguments"] = json.RawMessage(fmt.Sprintf(`{"session_id":%q,"node":"desk"}`, session))
	if refresh := chat.rpc("events/subscribe", ch); refresh["id"] != sub["id"] {
		t.Fatalf("refresh: %v %v", sub, refresh)
	}
	for _, pair := range []struct {
		client *mcpClient
		code   float64
		path   string
	}{{standard, -32027, "/badstandard"}, {chat, -32015, "/badchat"}} {
		bad := params(pair.path, r.secret(pair.path))
		r.mu.Lock()
		r.failChallenge[pair.path] = true
		r.mu.Unlock()
		_, out := pair.client.post(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "events/subscribe", "params": bad})
		e, ok := out["error"].(map[string]any)
		if !ok || e["code"] != pair.code || e["data"].(map[string]any)["reason"] == nil {
			t.Fatalf("challenge error: %v", out)
		}
		for _, target := range []string{"https://outside.example.test/callback", "http://127.0.0.1:1/callback"} {
			bad["delivery"].(map[string]any)["url"] = target
			_, out = pair.client.post(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "events/subscribe", "params": bad})
			e, ok = out["error"].(map[string]any)
			if !ok || e["code"] != pair.code || e["data"].(map[string]any)["reason"] == nil {
				t.Fatalf("origin error: %v", out)
			}
		}
	}
	r.mu.Lock()
	r.failDelivery["/a"] = true
	r.mu.Unlock()
	c.mustTool("send_agent_prompt", map[string]any{"node": "desk", "operation_id": "webhook-prompt", "session_id": session, "text": "webhook"})
	am, bm := r.wait("/a", 2), r.wait("/b", 1)
	r.wait("/expiry", 1)
	if len(am) != 2 || am[0].accepted || !am[1].accepted || am[0].id != am[1].id || am[0].body != am[1].body {
		t.Fatalf("retry: %v", am)
	}
	var av, bv map[string]any
	json.Unmarshal([]byte(am[1].body), &av)
	json.Unmarshal([]byte(bm[0].body), &bv)
	data, ok := av["data"].(map[string]any)
	if !ok || len(data) != 4 || data["node"] != "desk" || data["session_id"] != session || data["state"] != "idle" || data["observed_at"] == nil || av["eventId"] == nil || av["name"] != eventName || av["timestamp"] == nil || av["cursor"] == nil || av["eventId"] != bv["eventId"] || am[1].id == bm[0].id || am[1].id == av["eventId"] || bm[0].id == bv["eventId"] {
		t.Fatalf("occurrences: %v %v", av, bv)
	}
	bd, ok := bv["data"].(map[string]any)
	if !ok || len(bd) != 4 || !reflect.DeepEqual(bd, data) || bv["name"] != eventName || bv["timestamp"] != av["timestamp"] || bv["cursor"] == nil {
		t.Fatalf("second occurrence: %v", bv)
	}
	if out := c.mustTool("read_agent_output", map[string]any{"node": "desk", "session_id": session}).(map[string]any); out["reply"] != "echo: webhook" {
		t.Fatalf("output: %v", out)
	}
	r.wait("/chat", 1)
	d.kill()
	db, err := store.Open(filepath.Join(filepath.Dir(d.sock), "reach.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err = db.BeginTurn(core.Turn{ClientID: "e2e", OperationID: "webhook-unknown", SessionID: session}); err != nil {
		t.Fatal(err)
	}
	db.Close()
	d.start()
	c.waitTool("list_capabilities", map[string]any{"node": "desk"}, func(_ any, e *toolError) bool { return e == nil })
	cm := r.wait("/chat", 2)
	var unknown map[string]any
	json.Unmarshal([]byte(cm[1].body), &unknown)
	if unknown["data"].(map[string]any)["state"] != "unknown" || cm[1].id != unknown["eventId"] {
		t.Fatalf("chat unknown: %v", cm[1])
	}
	c.mustTool("read_agent_output", map[string]any{"node": "desk", "session_id": session})
	if out := chat.rpc("events/unsubscribe", ch); len(out) != 1 || out["resultType"] != "complete" {
		t.Fatalf("unsubscribe: %v", out)
	}
	c.mustTool("send_agent_prompt", map[string]any{"node": "desk", "operation_id": "webhook-after-unsubscribe", "session_id": session, "text": "later"})
	r.wait("/a", 4)
	time.Sleep(time.Second)
	if got, _ := r.snapshot("/chat"); len(got) != 2 {
		t.Fatalf("unsubscribed delivery: %v", got)
	}
	for _, path := range []string{"/badstandard", "/badchat"} {
		if got, _ := r.snapshot(path); len(got) != 0 {
			t.Fatalf("failed callback delivery: %v", got)
		}
	}
	c.mustTool("read_agent_output", map[string]any{"node": "desk", "session_id": session})
	// Replace the Worker while its push reader is open, retaining durable storage.
	refreshed := standard.rpc("events/subscribe", a)
	if refreshed["id"] != first["id"] {
		t.Fatalf("refresh id: %v %v", first, refreshed)
	}
	args := map[string]any{"node": "desk", "session_id": session}
	stream, cancel := standard.openEventStream(args, nil)
	active := readEvent(t, stream, "active")
	c.mustTool("send_agent_prompt", map[string]any{"node": "desk", "operation_id": "webhook-before-replacement", "session_id": session, "text": "before replacement"})
	oldEvent := readEvent(t, stream, "event")
	cursor := oldEvent["cursor"]
	c.mustTool("read_agent_output", map[string]any{"node": "desk", "session_id": session})
	_, controlsBefore := r.snapshot("/a")
	gateway.stop()
	ended := make(chan error, 1)
	go func() { _, err := io.Copy(io.Discard, stream.reader); ended <- err }()
	select {
	case <-ended:
	case <-time.After(10 * time.Second):
		t.Fatal("stream survived gateway replacement")
	}
	cancel()
	stream.Close()
	gateway.start()
	c.waitTool("list_capabilities", map[string]any{"node": "desk"}, func(_ any, e *toolError) bool { return e == nil })
	refreshed = standard.rpc("events/subscribe", a)
	if refreshed["id"] != first["id"] {
		t.Fatalf("replacement id: %v", refreshed)
	}
	// A grant or verification lost in the replacement would be recreated by
	// this refresh with a new challenge; durable state needs none.
	if _, controls := r.snapshot("/a"); len(controls) != len(controlsBefore) {
		t.Fatalf("replacement challenge: %v %v", controlsBefore, controls)
	}
	stream, cancel = standard.openEventStream(args, cursor)
	active = readEvent(t, stream, "active")
	if active["truncated"] != true {
		t.Fatalf("replacement stream: %v", active)
	}
	c.mustTool("send_agent_prompt", map[string]any{"node": "desk", "operation_id": "webhook-replacement", "session_id": session, "text": "replacement"})
	event := readEvent(t, stream, "event")
	if event["data"].(map[string]any)["session_id"] != session || event["eventId"] == oldEvent["eventId"] {
		t.Fatalf("replacement event: %v", event)
	}
	cancel()
	stream.Close()
	replacementMessages := r.wait("/a", 6)
	var replacement map[string]any
	if err := json.Unmarshal([]byte(replacementMessages[5].body), &replacement); err != nil || replacement["eventId"] != event["eventId"] {
		t.Fatalf("replacement webhook: %v %v", replacement, event)
	}
	if out := c.mustTool("read_agent_output", map[string]any{"node": "desk", "session_id": session}).(map[string]any); out["reply"] != "echo: replacement" {
		t.Fatalf("replacement output: %v", out)
	}
	fresh := params("/fresh", r.secret("/fresh"))
	standard.rpc("events/subscribe", fresh)
	if delay := time.Until(deadline.Add(time.Second)); delay > 0 {
		time.Sleep(delay)
	}
	before, _ := r.snapshot("/expiry")
	c.mustTool("send_agent_prompt", map[string]any{"node": "desk", "operation_id": "webhook-expired", "session_id": session, "text": "expiry"})
	r.wait("/fresh", 1)
	time.Sleep(time.Second)
	after, controls := r.snapshot("/expiry")
	if len(after) != len(before) {
		t.Fatalf("expired delivery: %v", after)
	}
	standard.rpc("events/subscribe", expiry)
	_, again := r.snapshot("/expiry")
	if len(again) != len(controls)+1 {
		t.Fatalf("expired verification: %v %v", controls, again)
	}
	c.mustTool("read_agent_output", map[string]any{"node": "desk", "session_id": session})
}

func TestWebhookReceiverRejectsInvalidSignature(t *testing.T) {
	r := newWebhookReceiver(t)
	r.secret("/invalid")
	req, err := http.NewRequest(http.MethodPost, r.server.URL+"/invalid", strings.NewReader(`{"type":"verification","challenge":"nonce"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("webhook-id", "message")
	req.Header.Set("webhook-timestamp", "1")
	req.Header.Set("X-MCP-Subscription-Id", "sub")
	req.Header.Set("webhook-signature", "v1,wrong")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("invalid signature: %s", resp.Status)
	}
	messages, controls := r.snapshot("/invalid")
	if len(messages) != 0 || len(controls) != 0 {
		t.Fatalf("invalid message recorded: %v %v", messages, controls)
	}
}
