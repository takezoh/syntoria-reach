// Package remote implements the small MCP client surface Reach needs for
// Controller tools. A client serializes requests because each connection has
// one response stream and monotonically increasing JSON-RPC IDs.
package remote

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// ControllerRPCError is a JSON-RPC error reply, distinct from losing the
// transport after a request may have reached the Controller.
type ControllerRPCError struct {
	Code    int
	Message string
}

func (e *ControllerRPCError) Error() string {
	return fmt.Sprintf("Controller JSON-RPC error %d: %s", e.Code, e.Message)
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Result  json.RawMessage `json:"result"`
	Error   *rpcError       `json:"error"`
}
type rpcRead struct {
	data []byte
	err  error
}

type MCPClient struct {
	mu                     sync.Mutex
	pendingMu              sync.Mutex
	cmd                    *exec.Cmd
	in                     io.WriteCloser
	out                    *bufio.Reader
	url, session, protocol string
	http                   *http.Client
	next                   uint64
	closed                 bool
	pending                map[string]chan rpcRead
	streamErr              error
}

// Stdio starts a child with a deliberately small environment and initializes
// MCP before returning. REACH_SOCKET and REACH_TOKEN are supplied explicitly.
func Stdio(ctx context.Context, command []string, socket, token string) (*MCPClient, error) {
	cmd := exec.CommandContext(ctx, command[0], command[1:]...)
	cmd.Stderr = io.Discard
	home, _ := os.UserHomeDir()
	cmd.Env = []string{"PATH=/usr/local/bin:/usr/bin:/bin", "LANG=C.UTF-8", "HOME=" + home}
	cmd.Env = append(cmd.Env, "REACH_SOCKET="+socket, "REACH_TOKEN="+token)
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	c := &MCPClient{cmd: cmd, in: in, out: bufio.NewReader(out), pending: map[string]chan rpcRead{}}
	go c.readLoop()
	initCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	err = c.initialize(initCtx)
	cancel()
	if err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		return nil, err
	}
	return c, nil
}

// HTTP connects to a resident Streamable HTTP MCP endpoint.
func HTTP(ctx context.Context, endpoint string) (*MCPClient, error) {
	c := &MCPClient{url: endpoint, http: &http.Client{Timeout: 30 * time.Second}}
	if err := c.initialize(ctx); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *MCPClient) initialize(ctx context.Context) error {
	var initialized struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	var raw json.RawMessage
	if err := c.request(ctx, "initialize", map[string]any{"protocolVersion": "2025-03-26", "capabilities": map[string]any{}, "clientInfo": map[string]string{"name": "syntoria-reach", "version": "0.2.0"}}, &raw); err != nil {
		return err
	}
	if err := json.Unmarshal(raw, &initialized); err != nil || initialized.ProtocolVersion == "" {
		return errors.New("MCP initialize returned no protocol version")
	}
	c.protocol = initialized.ProtocolVersion
	return c.notify(ctx, "notifications/initialized", map[string]any{})
}

func (c *MCPClient) Tools(ctx context.Context) (json.RawMessage, error) {
	var result json.RawMessage
	err := c.request(ctx, "tools/list", map[string]any{}, &result)
	return result, err
}

func (c *MCPClient) CallTool(ctx context.Context, name string, args json.RawMessage) (json.RawMessage, error) {
	var result json.RawMessage
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}
	err := c.request(ctx, "tools/call", map[string]json.RawMessage{"name": mustJSON(name), "arguments": args}, &result)
	return result, err
}

func mustJSON(v any) json.RawMessage { b, _ := json.Marshal(v); return b }

func (c *MCPClient) request(ctx context.Context, method string, params any, result *json.RawMessage) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return errors.New("connection closed")
	}
	c.next++
	id, _ := json.Marshal(c.next)
	msg, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(id), "method": method, "params": params})
	var data []byte
	if c.cmd != nil {
		response := make(chan rpcRead, 1)
		key := string(id)
		if err := c.register(key, response); err != nil {
			return err
		}
		if _, err := c.in.Write(append(msg, '\n')); err != nil {
			c.unregister(key, response)
			c.fail(err)
			return err
		}
		select {
		case r := <-response:
			if r.err != nil {
				return r.err
			}
			data = r.data
		case <-ctx.Done():
			c.unregister(key, response)
			return ctx.Err()
		}
	} else {
		var err error
		data, err = c.post(ctx, msg)
		if err != nil {
			return err
		}
	}
	var response rpcResponse
	if err := json.Unmarshal(data, &response); err != nil {
		return fmt.Errorf("invalid MCP response: %w", err)
	}
	if !bytes.Equal(bytes.TrimSpace(response.ID), id) {
		return fmt.Errorf("MCP %s response ID mismatch", method)
	}
	if response.Error != nil {
		return &ControllerRPCError{Code: response.Error.Code, Message: response.Error.Message}
	}
	if len(response.Result) == 0 {
		return fmt.Errorf("MCP %s returned no result", method)
	}
	*result = response.Result
	return nil
}

// readLoop is the sole owner of the stdio response stream. Canceled requests
// are removed from pending; their later replies are discarded by ID, never
// consumed as the response for a subsequent request.
func (c *MCPClient) readLoop() {
	for {
		line, err := c.out.ReadBytes('\n')
		if err != nil {
			c.fail(err)
			return
		}
		var response rpcResponse
		if err := json.Unmarshal(line, &response); err != nil {
			c.fail(fmt.Errorf("invalid MCP response: %w", err))
			return
		}
		key := string(bytes.TrimSpace(response.ID))
		if key == "" {
			if response.JSONRPC == "2.0" && response.Method != "" {
				continue // a server notification is not a response to a pending request
			}
			c.fail(errors.New("MCP response has no ID"))
			return
		}
		c.pendingMu.Lock()
		ch := c.pending[key]
		delete(c.pending, key)
		c.pendingMu.Unlock()
		if ch != nil {
			ch <- rpcRead{data: bytes.TrimSpace(line)}
		}
	}
}

func (c *MCPClient) register(id string, ch chan rpcRead) error {
	c.pendingMu.Lock()
	defer c.pendingMu.Unlock()
	if c.streamErr != nil {
		return c.streamErr
	}
	c.pending[id] = ch
	return nil
}

func (c *MCPClient) unregister(id string, ch chan rpcRead) {
	c.pendingMu.Lock()
	defer c.pendingMu.Unlock()
	if c.pending[id] == ch {
		delete(c.pending, id)
	}
}

func (c *MCPClient) fail(err error) {
	c.pendingMu.Lock()
	if c.streamErr != nil {
		c.pendingMu.Unlock()
		return
	}
	c.streamErr = err
	pending := c.pending
	c.pending = map[string]chan rpcRead{}
	c.pendingMu.Unlock()
	for _, ch := range pending {
		ch <- rpcRead{err: err}
	}
}

func (c *MCPClient) notify(ctx context.Context, method string, params any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	msg, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
	if c.cmd != nil {
		_, err := c.in.Write(append(msg, '\n'))
		return err
	}
	_, err := c.post(ctx, msg)
	return err
}

func (c *MCPClient) post(ctx context.Context, body []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if c.session != "" {
		req.Header.Set("Mcp-Session-Id", c.session)
	}
	if c.protocol != "" {
		req.Header.Set("MCP-Protocol-Version", c.protocol)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("MCP HTTP status %s", resp.Status)
	}
	if s := resp.Header.Get("Mcp-Session-Id"); s != "" {
		c.session = s
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, err
	}
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		for _, line := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(line, "data:") {
				return []byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), nil
			}
		}
		return nil, errors.New("MCP event stream contained no data")
	}
	return b, nil
}

func (c *MCPClient) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	if c.cmd == nil {
		return nil
	}
	c.in.Close()
	if c.cmd.Process != nil {
		c.cmd.Process.Kill()
	}
	_ = c.cmd.Wait() // a killed child exits non-zero by design
	return nil
}
