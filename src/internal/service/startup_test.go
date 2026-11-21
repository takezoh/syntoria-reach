package service_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/takezoh/syntoria-reach/src/internal/core"
	"github.com/takezoh/syntoria-reach/src/internal/store"
)

func TestStartupStopsAdmissionUntilPendingReconciled(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the daemon")
	}
	dir, err := os.MkdirTemp("/tmp", "reach-start-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	bin := filepath.Join(dir, "reach")
	build := exec.Command("go", "build", "-buildvcs=false", "-o", bin, "../../cmd/reach")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v %s", err, out)
	}
	path := filepath.Join(dir, "state.db")
	sock := filepath.Join(dir, "reach.sock")
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	turn := core.Turn{ClientID: "client", OperationID: "op", SessionID: "session"}
	if err := db.BeginTurn(turn); err != nil {
		t.Fatal(err)
	}
	db.Close()
	sqlDB, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer sqlDB.Close()
	if _, err = sqlDB.Exec(`CREATE TRIGGER reject_unknown BEFORE INSERT ON turn_stops BEGIN SELECT RAISE(FAIL, 'injected outbox failure'); END`); err != nil {
		t.Fatal(err)
	}
	cfg := map[string]any{"socket": sock, "database": path, "workspaces": filepath.Join(dir, "workspaces"), "clients": []any{map[string]any{"id": "client", "token": tokenA, "allow": []string{"agent.run"}}}}
	raw, _ := json.Marshal(cfg)
	configPath := filepath.Join(dir, "config.json")
	if err = os.WriteFile(configPath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "-config", configPath)
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil || err == nil || !strings.Contains(string(out), "injected outbox failure") {
		t.Fatalf("startup should fail on outbox write: %v %s", err, out)
	}
	if _, err = os.Stat(sock); !os.IsNotExist(err) {
		t.Fatalf("socket exposed after failed reconciliation: %v", err)
	}
	db, err = store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := db.PendingTurns()
	if err != nil || len(pending) != 1 || pending[0] != turn {
		t.Fatalf("pending: %v %v", pending, err)
	}
	stops, err := db.PendingTurnStops()
	if err != nil || len(stops) != 0 {
		t.Fatalf("stops after rollback: %v %v", stops, err)
	}
	db.Close()
	if _, err = sqlDB.Exec(`DROP TRIGGER reject_unknown`); err != nil {
		t.Fatal(err)
	}
	start := func() {
		t.Helper()
		os.Remove(sock)
		cmd := exec.Command(bin, "-config", configPath)
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		defer func() { cmd.Process.Kill(); cmd.Wait() }()
		deadline := time.Now().Add(5 * time.Second)
		for {
			if _, err := os.Stat(sock); err == nil {
				return
			}
			if time.Now().After(deadline) {
				t.Fatal("daemon did not start")
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	start()
	db, err = store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	stops, err = db.PendingTurnStops()
	if err != nil || len(stops) != 1 || stops[0].State != core.TurnUnknown || stops[0].SessionID != turn.SessionID {
		t.Fatalf("reconciled: %v %v", stops, err)
	}
	if err = db.DeleteTurnStop(stops[0].EventID); err != nil {
		t.Fatal(err)
	}
	db.Close()
	start()
	db, err = store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	stops, err = db.PendingTurnStops()
	if err != nil || len(stops) != 0 {
		t.Fatalf("repeated startup: %v %v", stops, err)
	}
}

func TestGatewayUnknownInstructions(t *testing.T) {
	data, err := os.ReadFile("../../../cloud/src/mcp.ts")
	if err != nil {
		t.Fatal(err)
	}
	source := string(data)
	begin := strings.Index(source, "const INSTRUCTIONS = `")
	if begin < 0 {
		t.Fatal("instructions missing")
	}
	source = source[begin+len("const INSTRUCTIONS = `"):]
	end := strings.Index(source, "`;")
	if end < 0 {
		t.Fatal("instructions unterminated")
	}
	instructions := source[:end]
	for _, phrase := range []string{`"unknown" turn-stop event`, `result_unknown from send_agent_prompt`, `call read_agent_output once for that session`, `If it is working, wait for the next event`, `If it is idle and its reply does not answer the prompt you sent`, `send it again with a new operation_id`, `Otherwise do not resend the prompt`, `No re-subscription is needed`} {
		if !strings.Contains(instructions, phrase) {
			t.Errorf("instructions missing %q", phrase)
		}
	}
	if !strings.Contains(source[end:], "{ instructions: INSTRUCTIONS }") {
		t.Fatal("instructions not supplied to MCP server")
	}
}
