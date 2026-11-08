// Package agenttest provides fake agent processes for tests. A test binary
// becomes a fake agent by calling MaybeRun when REACH_FAKE_AGENT is set.
package agenttest

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"testing"
	"time"
)

// Agent returns the command and environment that run the current test binary
// as a fake agent for protocol ("codex", "claude" or "acp"). The binary
// must call MaybeRun from a test selected by testName. Arguments Reach
// appends (Claude flags) follow "--", so the test binary does not parse them.
func Agent(t testing.TB, protocol, testName string) ([]string, map[string]string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := exec.LookPath(exe); err != nil {
		t.Fatal(err)
	}
	return []string{exe, "-test.run=^" + testName + "$", "--"}, map[string]string{"REACH_FAKE_AGENT": protocol}
}

// MaybeRun acts as a fake Codex app-server ("codex"), Claude Code stream-json
// process ("claude") or ACP agent ("acp") and exits when REACH_FAKE_AGENT is
// set; otherwise it returns. Prompt texts steer the fake: "hang" waits for
// a cancel, "crash" exits, and for Claude "fail" fails the turn. With
// REACH_FAKE_SLOW_START set, the fake waits a second before it reads its
// input. If REACH_FAKE_PID_FILE is set, the fake writes its PID there.
func MaybeRun() {
	if path := os.Getenv("REACH_FAKE_PID_FILE"); path != "" {
		os.WriteFile(path, []byte(fmt.Sprint(os.Getpid())), 0o644)
	}
	if os.Getenv("REACH_FAKE_AGENT") != "" && os.Getenv("REACH_FAKE_SLOW_START") != "" {
		time.Sleep(time.Second)
	}
	switch os.Getenv("REACH_FAKE_AGENT") {
	case "codex":
		fakeCodex()
	case "claude":
		fakeClaude()
	case "acp":
		fakeACP()
	default:
		return
	}
	os.Exit(0)
}

type fakeIO struct {
	in  *bufio.Scanner
	out *json.Encoder
}

func newFakeIO() *fakeIO {
	return &fakeIO{in: bufio.NewScanner(os.Stdin), out: json.NewEncoder(os.Stdout)}
}

func (f *fakeIO) next() (map[string]any, bool) {
	for f.in.Scan() {
		var m map[string]any
		if json.Unmarshal(f.in.Bytes(), &m) == nil {
			return m, true
		}
	}
	return nil, false
}

func (f *fakeIO) send(m map[string]any) { f.out.Encode(m) }

func params(m map[string]any) map[string]any { p, _ := m["params"].(map[string]any); return p }

func fakeCodex() {
	f := newFakeIO()
	thread, turns := "", 0
	var hanging string
	for {
		m, ok := f.next()
		if !ok {
			return
		}
		id, p := m["id"], params(m)
		switch m["method"] {
		case "initialize":
			f.send(map[string]any{"id": id, "result": map[string]any{}})
		case "thread/start":
			thread = "thr_1"
			f.send(map[string]any{"id": id, "result": map[string]any{"thread": map[string]any{"id": thread}}})
		case "turn/start":
			turns++
			turn := fmt.Sprintf("turn_%d", turns)
			text := p["input"].([]any)[0].(map[string]any)["text"].(string)
			f.send(map[string]any{"id": id, "result": map[string]any{"turn": map[string]any{"id": turn}}})
			switch text {
			case "crash":
				os.Exit(3)
			case "hang":
				hanging = turn
				continue
			}
			// Ask for an approval; Reach must reject it.
			f.send(map[string]any{"id": 900, "method": "item/commandExecution/requestApproval", "params": map[string]any{}})
			reply, _ := f.next()
			approval := "approved"
			if reply["error"] != nil {
				approval = "rejected"
			}
			msg := fmt.Sprintf("echo %s (approval=%s)", text, approval)
			f.send(map[string]any{"method": "item/completed", "params": map[string]any{
				"threadId": thread, "turnId": turn, "item": map[string]any{"type": "agentMessage", "text": msg},
			}})
			f.send(map[string]any{"method": "turn/completed", "params": map[string]any{
				"threadId": thread, "turn": map[string]any{"id": turn, "status": "completed"},
			}})
		case "thread/unsubscribe":
			f.send(map[string]any{"id": id, "result": map[string]any{"status": "unsubscribed"}})
		case "turn/interrupt":
			f.send(map[string]any{"id": id, "result": map[string]any{}})
			if hanging != "" {
				f.send(map[string]any{"method": "turn/completed", "params": map[string]any{
					"threadId": thread, "turn": map[string]any{"id": hanging, "status": "interrupted"},
				}})
				hanging = ""
			}
		}
	}
}

func fakeACP() {
	f := newFakeIO()
	session := ""
	var hanging any
	for {
		m, ok := f.next()
		if !ok {
			return
		}
		id, p := m["id"], params(m)
		reply := func(result any) { f.send(map[string]any{"jsonrpc": "2.0", "id": id, "result": result}) }
		update := func(text string) {
			f.send(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{
				"sessionId": session, "update": map[string]any{
					"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": text},
				},
			}})
		}
		switch m["method"] {
		case "initialize":
			reply(map[string]any{"protocolVersion": 1, "agentCapabilities": map[string]any{"sessionCapabilities": map[string]any{"close": map[string]any{}}}})
		case "session/new":
			session = "sess_1"
			reply(map[string]any{"sessionId": session})
		case "session/close":
			reply(nil)
		case "session/prompt":
			text := p["prompt"].([]any)[0].(map[string]any)["text"].(string)
			switch text {
			case "crash":
				os.Exit(3)
			case "hang":
				hanging = id
				continue
			}
			f.send(map[string]any{"jsonrpc": "2.0", "id": 900, "method": "session/request_permission", "params": map[string]any{
				"sessionId": session, "toolCall": map[string]any{},
				"options": []any{
					map[string]any{"optionId": "yes", "kind": "allow_once", "name": "Allow"},
					map[string]any{"optionId": "no", "kind": "reject_once", "name": "Reject"},
				},
			}})
			resp, _ := f.next()
			outcome := resp["result"].(map[string]any)["outcome"].(map[string]any)
			update("echo ")
			update(fmt.Sprintf("%s (permission=%v)", text, outcome["outcome"]))
			reply(map[string]any{"stopReason": "end_turn"})
		case "session/cancel":
			if hanging != nil {
				f.send(map[string]any{"jsonrpc": "2.0", "id": hanging, "result": map[string]any{"stopReason": "cancelled"}})
				hanging = nil
			}
		}
	}
}

// fakeClaude follows the stream-json exchange DaVinci's Claude client relies
// on: initialize control request, system/init with the session from
// --session-id, then one result per user message.
func fakeClaude() {
	session := ""
	for i, a := range os.Args {
		if a == "--session-id" && i+1 < len(os.Args) {
			session = os.Args[i+1]
		}
	}
	f := newFakeIO()
	initMsg := map[string]any{"type": "system", "subtype": "init", "session_id": session}
	for {
		m, ok := f.next()
		if !ok {
			return
		}
		switch m["type"] {
		case "control_request":
			f.send(initMsg)
			f.send(map[string]any{"type": "control_response", "response": map[string]any{
				"subtype": "success", "request_id": m["request_id"],
			}})
		case "user":
			content := m["message"].(map[string]any)["content"].([]any)
			text := content[0].(map[string]any)["text"].(string)
			switch text {
			case "crash":
				os.Exit(3)
			case "hang":
				for f.in.Scan() { // until stdin closes
				}
				return
			case "fail":
				f.send(map[string]any{"type": "result", "subtype": "error_during_execution", "is_error": true, "result": "boom", "session_id": session})
				continue
			}
			f.send(map[string]any{"type": "assistant", "session_id": session})
			f.send(map[string]any{"type": "result", "subtype": "success", "session_id": session, "result": "echo " + text})
		}
	}
}
