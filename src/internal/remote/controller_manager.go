// This file owns the node's long-lived MCP Controller connections.
package remote

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/takezoh/syntoria-reach/src/internal/core"
)

type ControllerSpec struct {
	ID, Role    string
	Command     []string
	URL, Client string
}

type controllerEntry struct {
	config ControllerSpec
	client *MCPClient
	err    error
	token  string
}
type ControllerManager struct {
	mu      sync.RWMutex
	entries map[string]*controllerEntry
	allow   map[string]bool
}

func NewControllerManager(specs []ControllerSpec, permissions []string) *ControllerManager {
	m := &ControllerManager{entries: map[string]*controllerEntry{}, allow: map[string]bool{}}
	for _, permission := range permissions {
		m.allow[permission] = true
	}
	for _, c := range specs {
		cp := c
		m.entries[c.ID] = &controllerEntry{config: cp}
	}
	return m
}

// Start opens each configured connection once. Initialization failures are
// retained and reported to requests for that Controller.
func (m *ControllerManager) StartControllers(ctx context.Context, socket string, clients map[string]string) {
	ids := make([]string, 0, len(m.entries))
	for id := range m.entries {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		e := m.entries[id]
		c := e.config
		e.token = clients[c.Client]
		var client *MCPClient
		var err error
		if len(c.Command) > 0 {
			client, err = Stdio(ctx, c.Command, socket, clients[c.Client])
		} else {
			client, err = HTTP(ctx, c.URL)
		}
		m.mu.Lock()
		e.client, e.err = client, err
		m.mu.Unlock()
	}
}

func (m *ControllerManager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, e := range m.entries {
		if e.client != nil {
			_ = e.client.Close()
		}
	}
}

func (m *ControllerManager) List() []map[string]string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]map[string]string, 0, len(m.entries))
	for _, e := range m.entries {
		if !m.mayListController(e.config.ID) {
			continue
		}
		out = append(out, map[string]string{"id": e.config.ID, "role": e.config.Role})
	}
	sort.Slice(out, func(i, j int) bool { return out[i]["id"] < out[j]["id"] })
	return out
}

func (m *ControllerManager) mayListController(id string) bool {
	if m.allow[core.ControllerPermission(id)] {
		return true
	}
	prefix := core.ControllerToolPermissionPrefix(id)
	for permission := range m.allow {
		if strings.HasPrefix(permission, prefix) && len(permission) > len(prefix) {
			return true
		}
	}
	return false
}

func (m *ControllerManager) mayCall(id, tool string) bool {
	return m.allow[core.ControllerPermission(id)] || m.allow[core.ControllerToolPermission(id, tool)]
}

func (m *ControllerManager) get(id string) (*controllerEntry, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	e, ok := m.entries[id]
	if !ok {
		return nil, core.Errorf(core.CodeNotFound, "unknown Controller %q", id)
	}
	if e.err != nil {
		return nil, core.Errorf(core.CodeHarnessFailed, "Controller %q is unavailable", id)
	}
	if e.client == nil {
		return nil, core.Errorf(core.CodeHarnessFailed, "Controller %q is unavailable", id)
	}
	return e, nil
}

func (m *ControllerManager) Tools(ctx context.Context, id string) (json.RawMessage, error) {
	e, err := m.get(id)
	if err != nil {
		return nil, err
	}
	raw, err := e.client.Tools(ctx)
	if err != nil {
		return nil, core.Errorf(core.CodeHarnessFailed, "Controller %q tools are unavailable", id)
	}
	if m.allow[core.ControllerPermission(id)] {
		return redactSecret(raw, e.token), nil
	}
	var listing struct {
		Tools []json.RawMessage `json:"tools"`
	}
	if err := json.Unmarshal(raw, &listing); err != nil {
		return nil, core.Errorf(core.CodeHarnessFailed, "Controller %q returned an invalid tools list", id)
	}
	filtered := listing.Tools[:0]
	for _, rawTool := range listing.Tools {
		var tool struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(rawTool, &tool) == nil && tool.Name != "" && m.mayCall(id, tool.Name) {
			filtered = append(filtered, rawTool)
		}
	}
	encoded, err := json.Marshal(struct {
		Tools []json.RawMessage `json:"tools"`
	}{Tools: filtered})
	if err != nil {
		return nil, err
	}
	return redactSecret(encoded, e.token), nil
}
func (m *ControllerManager) Call(ctx context.Context, id, tool string, args json.RawMessage) (json.RawMessage, error) {
	if !m.mayCall(id, tool) {
		return nil, core.Errorf(core.CodePermissionDenied, "client is not allowed to call Controller %q tool %q", id, tool)
	}
	e, err := m.get(id)
	if err != nil {
		return nil, err
	}
	raw, err := e.client.CallTool(ctx, tool, args)
	if err != nil {
		var rpcErr *ControllerRPCError
		if errors.As(err, &rpcErr) {
			var message string
			_ = json.Unmarshal(redactSecret(mustJSON(rpcErr.Message), e.token), &message)
			return nil, core.Errorf(core.CodeControllerError, "Controller %q rejected tool %q: %s", id, tool, message)
		}
		return nil, core.Errorf(core.CodeResultUnknown, "Controller %q tool %q may have run; Reach keeps no receipt of Controller calls, so check its effect with the Controller's own tools before calling it again", id, tool)
	}
	return redactSecret(raw, e.token), nil
}

func redactSecret(raw json.RawMessage, token string) json.RawMessage {
	if token == "" || len(raw) == 0 {
		return raw
	}
	redacted := []byte("[redacted]")
	raw = bytes.ReplaceAll(raw, []byte(token), redacted)
	encoded, _ := json.Marshal(token)
	if len(encoded) > 1 {
		raw = bytes.ReplaceAll(raw, encoded[1:len(encoded)-1], redacted)
	}
	return raw
}

type controllerRequest struct {
	Controller string          `json:"controller"`
	Tool       string          `json:"tool"`
	Arguments  json.RawMessage `json:"arguments"`
}

func (m *ControllerManager) Handle(ctx context.Context, method string, body []byte) (any, error) {
	switch method {
	case "list_controllers":
		return map[string]any{"controllers": m.List()}, nil
	case "list_controller_tools":
		var r struct {
			Controller string `json:"controller"`
		}
		if err := json.Unmarshal(body, &r); err != nil || r.Controller == "" {
			return nil, core.Errorf(core.CodeInvalidRequest, "controller is required")
		}
		if !m.mayListController(r.Controller) {
			return nil, core.Errorf(core.CodePermissionDenied, "client is not allowed to list Controller %q", r.Controller)
		}
		raw, err := m.Tools(ctx, r.Controller)
		if err != nil {
			return nil, err
		}
		return json.RawMessage(raw), nil
	case "call_controller_tool":
		var r controllerRequest
		if err := json.Unmarshal(body, &r); err != nil || r.Controller == "" || r.Tool == "" {
			return nil, core.Errorf(core.CodeInvalidRequest, "controller and tool are required")
		}
		raw, err := m.Call(ctx, r.Controller, r.Tool, r.Arguments)
		if err != nil {
			return nil, err
		}
		return json.RawMessage(raw), nil
	default:
		return nil, fmt.Errorf("unknown Controller method %q", method)
	}
}
