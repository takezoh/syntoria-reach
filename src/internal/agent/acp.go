package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"

	"github.com/takezoh/syntoria-reach/src/internal/core"
)

// acp speaks the Agent Client Protocol (v1), following DaVinci's DSH client,
// as a client that offers no file system or terminal capabilities: the agent
// uses its own tools and sandbox. Permission requests are denied unless
// allowPermissions is set.
type acp struct {
	rpc              *rpc
	allowPermissions bool
	closeCap         bool

	mu        sync.Mutex
	session   string
	prompting bool
	emit      func(typ, text string)
	text      strings.Builder
}

func newACP(w io.WriteCloser, r io.Reader, allowPermissions bool) *acp {
	c := &acp{allowPermissions: allowPermissions}
	c.rpc = newRPC(w, r, "2.0", c.handle)
	return c
}

func (c *acp) done() <-chan struct{} { return c.rpc.done }

func (c *acp) handle(m message) {
	switch m.Method {
	case "session/update":
		var p struct {
			SessionID string `json:"sessionId"`
			Update    struct {
				Kind    string `json:"sessionUpdate"`
				Content struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"update"`
		}
		if json.Unmarshal(m.Params, &p) != nil {
			return
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		if !c.prompting || p.SessionID != c.session {
			return // e.g. history replayed by session/load
		}
		if p.Update.Kind == "agent_message_chunk" && p.Update.Content.Type == "text" {
			c.text.WriteString(p.Update.Content.Text)
		} else {
			c.flushLocked()
		}
	case "session/request_permission":
		c.rpc.reply(m.ID, map[string]any{"outcome": c.permission(m.Params)}, nil)
	default:
		if len(m.ID) > 0 {
			c.rpc.unsupported(m)
		}
	}
}

func (c *acp) permission(params json.RawMessage) any {
	cancelled := map[string]string{"outcome": "cancelled"}
	if !c.allowPermissions {
		return cancelled
	}
	var p struct {
		Options []struct {
			OptionID string `json:"optionId"`
			Kind     string `json:"kind"`
		} `json:"options"`
	}
	if json.Unmarshal(params, &p) != nil {
		return cancelled
	}
	for _, o := range p.Options {
		if o.Kind == "allow_once" {
			return map[string]string{"outcome": "selected", "optionId": o.OptionID}
		}
	}
	return cancelled
}

// flushLocked emits buffered message chunks as one message event.
func (c *acp) flushLocked() {
	if c.text.Len() > 0 && c.emit != nil {
		c.emit(core.EventMessage, c.text.String())
	}
	c.text.Reset()
}

func (c *acp) open(ctx context.Context, cwd, resume string) (string, error) {
	var init struct {
		ProtocolVersion   int `json:"protocolVersion"`
		AgentCapabilities struct {
			LoadSession bool                       `json:"loadSession"`
			Sessions    map[string]json.RawMessage `json:"sessionCapabilities"`
		} `json:"agentCapabilities"`
	}
	if err := c.rpc.call(ctx, "initialize", map[string]any{
		"protocolVersion":    1,
		"clientCapabilities": map[string]any{"fs": map[string]bool{"readTextFile": false, "writeTextFile": false}, "terminal": false},
		"clientInfo":         map[string]string{"name": "syntoria-reach", "version": "0"},
	}, &init); err != nil {
		return "", err
	}
	if init.ProtocolVersion != 1 {
		return "", errors.New("agent does not support ACP version 1")
	}
	_, c.closeCap = init.AgentCapabilities.Sessions["close"]
	if resume != "" {
		// session/resume (used by DSH) continues without replay; session/load
		// replays history, which handle ignores outside a prompt.
		method := "session/resume"
		if _, ok := init.AgentCapabilities.Sessions["resume"]; !ok {
			if !init.AgentCapabilities.LoadSession {
				return "", errors.New("agent cannot resume sessions")
			}
			method = "session/load"
		}
		c.setSession(resume)
		if err := c.rpc.call(ctx, method, map[string]any{"sessionId": resume, "cwd": cwd, "mcpServers": []any{}}, nil); err != nil {
			return "", err
		}
		return resume, nil
	}
	var res struct {
		SessionID string `json:"sessionId"`
	}
	if err := c.rpc.call(ctx, "session/new", map[string]any{"cwd": cwd, "mcpServers": []any{}}, &res); err != nil {
		return "", err
	}
	if res.SessionID == "" {
		return "", errors.New("agent returned no session id")
	}
	c.setSession(res.SessionID)
	return res.SessionID, nil
}

func (c *acp) setSession(id string) {
	c.mu.Lock()
	c.session = id
	c.mu.Unlock()
}

func (c *acp) prompt(ctx context.Context, text string, emit func(typ, text string)) (string, error) {
	c.mu.Lock()
	c.prompting, c.emit = true, emit
	session := c.session
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.flushLocked()
		c.prompting, c.emit = false, nil
		c.mu.Unlock()
	}()
	var res struct {
		StopReason string `json:"stopReason"`
	}
	err := c.rpc.call(ctx, "session/prompt", map[string]any{
		"sessionId": session,
		"prompt":    []map[string]string{{"type": "text", "text": text}},
	}, &res)
	return res.StopReason, err
}

// close cancels a running prompt, closes the session if the agent supports
// it and closes the harness input.
func (c *acp) close(ctx context.Context) error {
	c.mu.Lock()
	session, prompting := c.session, c.prompting
	c.mu.Unlock()
	var err error
	if prompting {
		err = c.rpc.notify("session/cancel", map[string]string{"sessionId": session})
	}
	if session != "" && c.closeCap && err == nil {
		err = c.rpc.call(ctx, "session/close", map[string]string{"sessionId": session}, nil)
	}
	return errors.Join(err, c.rpc.w.Close())
}
