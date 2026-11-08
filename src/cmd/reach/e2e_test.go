package main

// End-to-end test against the real binary: standalone use over the Unix
// socket, idempotent resend, survival of a client disconnect, background jobs,
// agent sessions (with a fake Codex app-server), and a daemon crash that
// kills its children and leaves their outcome unknown (never re-executed).

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/takezoh/syntoria-reach/src/internal/agent/agenttest"
	"github.com/takezoh/syntoria-reach/src/internal/orca/orcatest"
)

const e2eToken = "e2e-token-0123456789abcdef0123456789abcdef"

type daemon struct {
	t      *testing.T
	bin    string
	config string
	sock   string
	cmd    *exec.Cmd
	client *http.Client
}

// newDaemon builds the daemon and its config. extra holds more top-level
// config fields, each ending with a comma.
func newDaemon(t *testing.T, extra ...string) (*daemon, string) {
	t.Helper()
	// Unix socket paths are limited to ~108 bytes, so avoid long temp dirs.
	dir, err := os.MkdirTemp("/tmp", "reach-e2e-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })

	bin := filepath.Join(dir, "reach")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	orcaRoot := filepath.Join(dir, "orca")
	if err := os.Mkdir(orcaRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	ws := filepath.Join(orcaRoot, "ws") // created by the fake Orca
	sock := filepath.Join(dir, "reach.sock")
	agentCmd, agentEnv := agenttest.Agent(t, "codex", "TestFakeHarness")
	agentEnv["REACH_FAKE_PID_FILE"] = filepath.Join(ws, "agent.pid")
	agent, _ := json.Marshal(map[string]any{"id": "codex", "protocol": "codex", "command": agentCmd, "env": agentEnv})
	orca, _ := json.Marshal(orcatest.Command(t, "TestFakeHarness", orcaRoot))
	cfg := fmt.Sprintf(`{
		%s
		"socket": %q, "database": %q,
		"orca": %s,
		"agents": [%s],
		"projects": [{"id": "proj", "repo": "/repo", "commands": ["/bin/sh"], "agents": ["codex"]}],
		"clients": [{"id": "e2e", "token": %q,
			"grants": [{"project": "proj", "allow": ["workspace.create", "file.read", "file.write", "command.run", "agent.run"]}]}]
	}`, strings.Join(extra, "\n"), sock, filepath.Join(dir, "reach.db"), orca, agent, e2eToken)
	config := filepath.Join(dir, "config.json")
	if err := os.WriteFile(config, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	d := &daemon{t: t, bin: bin, config: config, sock: sock}
	d.client = &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sock)
		},
	}}
	t.Cleanup(d.kill)
	return d, ws
}

func (d *daemon) start() {
	d.t.Helper()
	os.Remove(d.sock)
	d.cmd = exec.Command(d.bin, "-config", d.config)
	d.cmd.Stderr = os.Stderr
	if err := d.cmd.Start(); err != nil {
		d.t.Fatal(err)
	}
	for range 100 {
		if _, err := os.Stat(d.sock); err == nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	d.t.Fatal("daemon did not create its socket")
}

// kill simulates a crash: no graceful shutdown.
func (d *daemon) kill() {
	if d.cmd != nil && d.cmd.ProcessState == nil {
		d.cmd.Process.Kill()
		d.cmd.Wait()
	}
}

func (d *daemon) call(ctx context.Context, method string, body any) (int, map[string]any, error) {
	b, _ := json.Marshal(body)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://reach/v1/"+method, bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+e2eToken)
	resp, err := d.client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out, nil
}

func (d *daemon) mustCall(method string, body any) map[string]any {
	d.t.Helper()
	status, out, err := d.call(context.Background(), method, body)
	if err != nil || status != http.StatusOK {
		d.t.Fatalf("%s: status=%d out=%v err=%v", method, status, out, err)
	}
	return out
}

// callList calls a method whose result is a JSON array.
func (d *daemon) callList(method string) []any {
	d.t.Helper()
	b, _ := json.Marshal(map[string]any{})
	req, _ := http.NewRequest(http.MethodPost, "http://reach/v1/"+method, bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+e2eToken)
	resp, err := d.client.Do(req)
	if err != nil {
		d.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out []any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || resp.StatusCode != http.StatusOK {
		d.t.Fatalf("%s: status=%d err=%v", method, resp.StatusCode, err)
	}
	return out
}

func (d *daemon) waitState(opID, want string) map[string]any {
	d.t.Helper()
	for range 100 {
		op := d.mustCall("get_operation", map[string]any{"operation_id": opID})
		if op["state"] == want {
			return op
		}
		time.Sleep(50 * time.Millisecond)
	}
	d.t.Fatalf("operation %s never reached %s", opID, want)
	return nil
}

// poll calls get until cond holds for its result.
func (d *daemon) poll(method string, body any, cond func(map[string]any) bool) map[string]any {
	d.t.Helper()
	for range 200 {
		out := d.mustCall(method, body)
		if cond(out) {
			return out
		}
		time.Sleep(25 * time.Millisecond)
	}
	d.t.Fatalf("%s %v: condition never held", method, body)
	return nil
}

func result(op map[string]any) map[string]any {
	r, _ := op["result"].(map[string]any)
	return r
}

// waitFile waits until path exists and returns its contents.
func waitFile(t *testing.T, path string) string {
	t.Helper()
	for range 200 {
		if b, err := os.ReadFile(path); err == nil && len(b) > 0 {
			return strings.TrimSpace(string(b))
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("%s was not written", path)
	return ""
}

// waitDead waits until the process is gone or a zombie.
func waitDead(t *testing.T, what, pid string) {
	t.Helper()
	for range 200 {
		stat, err := os.ReadFile("/proc/" + pid + "/stat")
		if err != nil {
			return
		}
		if f := strings.Fields(string(stat)); len(f) > 2 && f[2] == "Z" {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("%s (pid %s) survived the daemon", what, pid)
}

func lines(t *testing.T, path string) int {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(b), "\n")
}

func TestE2E(t *testing.T) {
	d, ws := newDaemon(t)
	d.start()

	// Workspaces are created on the Orca server through Reach.
	created := d.mustCall("create_workspace", map[string]any{"operation_id": "cw", "project": "proj", "name": "ws"})
	wsID := result(created)["workspace"]
	if created["state"] != "succeeded" || result(created)["path"] != ws {
		t.Fatalf("create_workspace: %v", created)
	}

	// Standalone use: write, then read back with the returned version.
	op := d.mustCall("write_file", map[string]any{
		"operation_id": "w1", "workspace": wsID, "path": "notes/a.txt", "content": []byte("hello"),
	})
	if op["state"] != "succeeded" {
		t.Fatalf("write_file: %v", op)
	}
	read := d.mustCall("read_file", map[string]any{"workspace": wsID, "path": "notes/a.txt"})
	if read["version"] != op["result"].(map[string]any)["version"] {
		t.Fatalf("read_file: %v", read)
	}

	// The client disconnects while the command runs; the command still
	// finishes once, and the outcome is reconciled with get_operation.
	run := map[string]any{
		"operation_id": "c1", "workspace": wsID, "executable": "/bin/sh",
		"args": []string{"-c", "sleep 0.5; echo ran >> runs.log"},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	if _, _, err := d.call(ctx, "run_command", run); err == nil {
		t.Fatal("expected the client to give up before the command finished")
	}
	cancel()
	d.waitState("c1", "succeeded")
	// Resending the same operation returns the stored result without rerunning.
	if op := d.mustCall("run_command", run); op["state"] != "succeeded" {
		t.Fatalf("resend: %v", op)
	}
	if n := lines(t, filepath.Join(ws, "runs.log")); n != 1 {
		t.Fatalf("command ran %d times, want 1", n)
	}

	// A background job outlives the request and its output is read by offset.
	op = d.mustCall("start_job", map[string]any{
		"operation_id": "j1", "workspace": wsID, "executable": "/bin/sh", "args": []string{"-c", "echo job-out"},
	})
	jobID := result(op)["job_id"]
	d.poll("get_job", map[string]any{"job_id": jobID}, func(j map[string]any) bool { return j["state"] == "succeeded" })
	out := d.mustCall("read_job_output", map[string]any{"job_id": jobID, "stream": "stdout"})
	if out["data"] != "am9iLW91dAo=" || out["eof"] != true { // "job-out\n"
		t.Fatalf("read_job_output: %v", out)
	}

	// An agent session: one turn, read back as events.
	op = d.mustCall("start_agent_session", map[string]any{"operation_id": "s1", "workspace": wsID, "agent": "codex"})
	if result(op)["state"] != "idle" {
		t.Fatalf("start_agent_session: %v", op)
	}
	sessionID := result(op)["session_id"]
	if op := d.mustCall("send_agent_prompt", map[string]any{"operation_id": "p1", "session_id": sessionID, "text": "hi"}); result(op)["turn"] != 1.0 {
		t.Fatalf("send_agent_prompt: %v", op)
	}
	events := d.poll("read_agent_events", map[string]any{"session_id": sessionID}, func(e map[string]any) bool { return e["state"] == "idle" })
	if ev := events["events"].([]any); len(ev) != 2 || !strings.HasPrefix(ev[0].(map[string]any)["text"].(string), "echo hi") {
		t.Fatalf("events: %v", events)
	}
	d.mustCall("stop_agent_session", map[string]any{"operation_id": "x1", "session_id": sessionID})
	os.Remove(filepath.Join(ws, "agent.pid"))

	// The daemon crashes while a command, a job and an agent turn are running.
	// Its children die with it; after restart their outcome is unknown, and
	// resending does not execute anything again.
	crash := map[string]any{
		"operation_id": "c2", "workspace": wsID, "executable": "/bin/sh",
		"args": []string{"-c", "echo $$ > crash.pid; echo started >> crash.log; exec sleep 30"},
	}
	go d.call(context.Background(), "run_command", crash)
	commandPID := waitFile(t, filepath.Join(ws, "crash.pid"))
	op = d.mustCall("start_job", map[string]any{
		"operation_id": "j2", "workspace": wsID, "executable": "/bin/sh", "args": []string{"-c", "echo $$ > job.pid; exec sleep 30"},
	})
	crashJob := result(op)["job_id"]
	jobPID := waitFile(t, filepath.Join(ws, "job.pid"))
	op = d.mustCall("start_agent_session", map[string]any{"operation_id": "s2", "workspace": wsID, "agent": "codex"})
	crashSession := result(op)["session_id"]
	agentPID := waitFile(t, filepath.Join(ws, "agent.pid"))
	d.mustCall("send_agent_prompt", map[string]any{"operation_id": "p2", "session_id": crashSession, "text": "hang"})

	d.kill()
	waitDead(t, "command", commandPID)
	waitDead(t, "job", jobPID)
	waitDead(t, "agent harness", agentPID)

	d.start()
	if op := d.waitState("c2", "unknown"); op["error_code"] != "result_unknown" {
		t.Fatalf("after crash: %v", op)
	}
	if op := d.mustCall("run_command", crash); op["state"] != "unknown" {
		t.Fatalf("resend after crash: %v", op)
	}
	if n := lines(t, filepath.Join(ws, "crash.log")); n != 1 {
		t.Fatalf("command started %d times, want 1", n)
	}
	if j := d.mustCall("get_job", map[string]any{"job_id": crashJob}); j["state"] != "unknown" {
		t.Fatalf("job after crash: %v", j)
	}
	if s := d.mustCall("get_agent_session", map[string]any{"session_id": crashSession}); s["state"] != "unknown" {
		t.Fatalf("agent session after crash: %v", s)
	}
	// Completed work survives the restart.
	d.waitState("c1", "succeeded")
	d.waitState("w1", "succeeded")
	if j := d.mustCall("get_job", map[string]any{"job_id": jobID}); j["state"] != "succeeded" {
		t.Fatalf("finished job after restart: %v", j)
	}

	// The workspace survives the restart and is deleted through Reach.
	if list := d.callList("list_workspaces"); len(list) != 1 {
		t.Fatalf("list_workspaces after restart: %v", list)
	}
	if op := d.mustCall("delete_workspace", map[string]any{"operation_id": "dw", "workspace": wsID}); op["state"] != "succeeded" {
		t.Fatalf("delete_workspace: %v", op)
	}
	if _, err := os.Stat(ws); !os.IsNotExist(err) {
		t.Fatal("worktree still exists after delete_workspace")
	}
}

// TestFakeHarness is not a test: it turns the test binary into a fake agent
// harness or a fake Orca CLI.
func TestFakeHarness(t *testing.T) {
	orcatest.MaybeRun()
	agenttest.MaybeRun()
	t.Skip("helper process")
}
