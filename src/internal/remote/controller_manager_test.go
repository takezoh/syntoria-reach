package remote

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/takezoh/syntoria-reach/src/internal/core"
)

func TestControllerManagerSharesInitializedConnectionAndHidesSecrets(t *testing.T) {
	var initialized, tools, calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
		}
		if len(req.ID) == 0 {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		var result string
		switch req.Method {
		case "initialize":
			initialized++
			result = `{"protocolVersion":"2025-03-26","capabilities":{},"serverInfo":{"name":"fixture","version":"1"}}`
		case "tools/list":
			tools++
			result = `{"tools":[{"name":"do_work","description":"token-sentinel"},{"name":"admin","description":"private"}]}`
		case "tools/call":
			calls++
			result = `{"content":[{"type":"text","text":"token-sentinel"}],"isError":true}`
		default:
			t.Errorf("unexpected MCP method %q", req.Method)
			result = `{}`
		}
		if req.Method == "initialize" {
			w.Header().Set("Mcp-Session-Id", "fixture-session")
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":%s}`, req.ID, result)
	}))
	defer srv.Close()
	manager := NewControllerManager([]ControllerSpec{{ID: "workflow", Role: "Runs workflow", URL: srv.URL, Client: "local"}}, []string{core.ControllerToolPermission("workflow", "do_work")})
	manager.StartControllers(context.Background(), "/run/reach.sock", map[string]string{"local": "token-sentinel"})
	defer manager.Close()
	listed, err := manager.Handle(context.Background(), "list_controllers", nil)
	if err != nil {
		t.Fatal(err)
	}
	listing, _ := json.Marshal(listed)
	if strings.Contains(string(listing), "token-sentinel") || !strings.Contains(string(listing), "workflow") {
		t.Fatalf("unsafe or incomplete listing: %s", listing)
	}
	visible, err := manager.Handle(context.Background(), "list_controller_tools", []byte(`{"controller":"workflow"}`))
	if err != nil {
		t.Fatal(err)
	}
	visibleJSON, _ := json.Marshal(visible)
	if !strings.Contains(string(visibleJSON), "do_work") || strings.Contains(string(visibleJSON), "admin") {
		t.Fatalf("tool list did not follow per-tool authority: %s", visibleJSON)
	}
	for _, method := range []string{"list_controller_tools", "call_controller_tool"} {
		var body string
		if method == "list_controller_tools" {
			body = `{"controller":"workflow"}`
		} else {
			body = `{"controller":"workflow","tool":"do_work","arguments":{"x":1}}`
		}
		got, err := manager.Handle(context.Background(), method, []byte(body))
		if err != nil {
			t.Fatalf("%s: %v", method, err)
		}
		encoded, _ := json.Marshal(got)
		if strings.Contains(string(encoded), "token-sentinel") {
			t.Fatalf("%s leaked token: %s", method, encoded)
		}
		if method == "call_controller_tool" && !strings.Contains(string(encoded), `"isError":true`) {
			t.Fatalf("Controller MCP error result was not preserved: %s", encoded)
		}
	}
	if _, err := manager.Handle(context.Background(), "call_controller_tool", []byte(`{"controller":"workflow","tool":"admin","arguments":{}}`)); err == nil || !strings.Contains(err.Error(), "permission_denied") {
		t.Fatalf("unauthorized call was not refused: %v", err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := manager.Handle(context.Background(), "call_controller_tool", []byte(`{"controller":"workflow","tool":"do_work","arguments":{}}`)); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if initialized != 1 || tools != 2 || calls != 6 {
		t.Fatalf("initialize=%d list=%d calls=%d", initialized, tools, calls)
	}
}

func TestControllerCallPreservesControllerIdentityAndReconcilesLostReply(t *testing.T) {
	operations := map[string]string{}
	effects := map[string]int{}
	calls := map[string]int{}
	lost := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			} `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			return
		}
		if len(req.ID) == 0 {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		var result string
		switch req.Method {
		case "initialize":
			result = `{"protocolVersion":"2025-03-26","capabilities":{},"serverInfo":{"name":"fixture","version":"1"}}`
		case "tools/call":
			var args struct {
				OperationID string `json:"operation_id"`
				Payload     string `json:"payload"`
			}
			if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
				t.Error(err)
				return
			}
			switch req.Params.Name {
			case "run":
				calls[args.OperationID]++
				if prior, ok := operations[args.OperationID]; ok && prior != args.Payload {
					fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"error":{"code":-32000,"message":"operation_conflict token-sentinel"}}`, req.ID)
					return
				}
				if _, ok := operations[args.OperationID]; !ok {
					operations[args.OperationID] = args.Payload
					effects[args.OperationID]++
				}
				if args.OperationID == "lost-op-1" && !lost {
					lost = true
					if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
						conn.Close()
					}
					return
				}
				result = `{"content":[{"type":"text","text":"accepted"}]}`
			case "get_status":
				var statusArgs struct {
					OperationID string `json:"operation_id"`
				}
				if err := json.Unmarshal(req.Params.Arguments, &statusArgs); err != nil {
					t.Error(err)
					return
				}
				resultBytes, _ := json.Marshal(map[string]any{"content": []any{}, "structuredContent": map[string]any{
					"operation_id": statusArgs.OperationID,
					"state":        "succeeded",
					"effect_count": effects[statusArgs.OperationID],
				}})
				result = string(resultBytes)
			default:
				t.Errorf("unexpected tool %q", req.Params.Name)
				return
			}
		case "tools/list":
			result = `{"tools":[]}`
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":%s}`, req.ID, result)
	}))
	defer srv.Close()
	manager := NewControllerManager([]ControllerSpec{{ID: "workflow", Role: "Runs workflow", URL: srv.URL, Client: "local"}}, []string{core.ControllerPermission("workflow")})
	manager.StartControllers(context.Background(), "", map[string]string{"local": "token-sentinel"})
	defer manager.Close()

	call := func(tool string, args map[string]string) (any, error) {
		body, _ := json.Marshal(map[string]any{"controller": "workflow", "tool": tool, "arguments": args})
		return manager.Handle(context.Background(), "call_controller_tool", body)
	}
	if _, err := call("run", map[string]string{"operation_id": "op-1", "payload": "first"}); err != nil {
		t.Fatalf("first operation call: %v", err)
	}
	_, conflictErr := call("run", map[string]string{"operation_id": "op-1", "payload": "changed"})
	if conflictErr == nil || !strings.Contains(conflictErr.Error(), core.CodeControllerError) || !strings.Contains(conflictErr.Error(), "operation_conflict") || strings.Contains(conflictErr.Error(), "token-sentinel") || effects["op-1"] != 1 {
		t.Fatalf("changed payload did not preserve the Controller conflict: err=%v effects=%d", conflictErr, effects["op-1"])
	}
	_, lostErr := call("run", map[string]string{"operation_id": "lost-op-1", "payload": "effect"})
	if lostErr == nil || !strings.Contains(lostErr.Error(), core.CodeResultUnknown) || calls["lost-op-1"] != 1 {
		t.Fatalf("lost reply did not remain unknown without a retry: err=%v calls=%d", lostErr, calls["lost-op-1"])
	}
	status, statusErr := call("get_status", map[string]string{"operation_id": "lost-op-1"})
	if statusErr != nil {
		t.Fatalf("Controller get_status: %v", statusErr)
	}
	var observed struct {
		StructuredContent struct {
			OperationID string `json:"operation_id"`
			State       string `json:"state"`
			EffectCount int    `json:"effect_count"`
		} `json:"structuredContent"`
	}
	if err := json.Unmarshal(status.(json.RawMessage), &observed); err != nil {
		t.Fatal(err)
	}
	if observed.StructuredContent.OperationID != "lost-op-1" || observed.StructuredContent.State != "succeeded" || observed.StructuredContent.EffectCount != 1 || calls["lost-op-1"] != 1 {
		t.Fatalf("Controller reconciliation did not show the single effect: status=%+v calls=%d", observed.StructuredContent, calls["lost-op-1"])
	}
}
