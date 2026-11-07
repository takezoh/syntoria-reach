package main

// End-to-end test of the Stage 1 acceptance criteria against the real
// binary: standalone use over the Unix socket, idempotent resend, survival of
// a client disconnect, and unknown (never re-executed) after a daemon crash.

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
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
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

func newDaemon(t *testing.T) (*daemon, string) {
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
	ws := filepath.Join(dir, "ws")
	if err := os.Mkdir(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(dir, "reach.sock")
	cfg := fmt.Sprintf(`{
		"socket": %q, "database": %q,
		"workspaces": [{"id": "ws", "root": %q, "commands": ["/bin/sh"]}],
		"clients": [{"id": "e2e", "token": %q,
			"grants": [{"workspace": "ws", "allow": ["file.read", "file.write", "command.run"]}]}]
	}`, sock, filepath.Join(dir, "reach.db"), ws, e2eToken)
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

	// Standalone use: write, then read back with the returned version.
	op := d.mustCall("write_file", map[string]any{
		"operation_id": "w1", "workspace": "ws", "path": "notes/a.txt", "content": []byte("hello"),
	})
	if op["state"] != "succeeded" {
		t.Fatalf("write_file: %v", op)
	}
	read := d.mustCall("read_file", map[string]any{"workspace": "ws", "path": "notes/a.txt"})
	if read["version"] != op["result"].(map[string]any)["version"] {
		t.Fatalf("read_file: %v", read)
	}

	// The client disconnects while the command runs; the command still
	// finishes once, and the outcome is reconciled with get_operation.
	run := map[string]any{
		"operation_id": "c1", "workspace": "ws", "executable": "/bin/sh",
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

	// The daemon crashes during a command. After restart the operation is
	// unknown, and resending it does not execute the command again.
	crash := map[string]any{
		"operation_id": "c2", "workspace": "ws", "executable": "/bin/sh",
		"args": []string{"-c", "echo $$ > crash.pid; echo started >> crash.log; sleep 30"},
	}
	go d.call(context.Background(), "run_command", crash)
	for range 100 {
		if _, err := os.Stat(filepath.Join(ws, "crash.pid")); err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	d.kill()
	// Stage 1 has no supervisor: the orphaned command keeps running. Stop it.
	if b, err := os.ReadFile(filepath.Join(ws, "crash.pid")); err == nil {
		if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
			syscall.Kill(-pid, syscall.SIGKILL)
		}
	}

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
	// Completed operations survive the restart.
	d.waitState("c1", "succeeded")
	d.waitState("w1", "succeeded")
}
