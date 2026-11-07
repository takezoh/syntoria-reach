package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
)

// message is one newline-delimited JSON-RPC message. Codex app-server omits
// the "jsonrpc" member; ACP requires "2.0".
type message struct {
	JSONRPC string          `json:"jsonrpc,omitempty"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// errClosed is returned by calls made after the harness stopped responding.
var errClosed = errors.New("harness connection closed")

// rpc is a JSON-RPC connection over a harness's stdin/stdout. handle receives
// notifications and requests from the harness on the reader goroutine; it may
// write (reply) but must not call.
type rpc struct {
	w       io.WriteCloser
	r       io.Reader
	version string
	handle  func(message)

	writeMu sync.Mutex
	mu      sync.Mutex
	next    int64
	pending map[int64]chan message
	done    chan struct{}
	err     error
}

func newRPC(w io.WriteCloser, r io.Reader, version string, handle func(message)) *rpc {
	c := &rpc{w: w, r: r, version: version, handle: handle, pending: map[int64]chan message{}, done: make(chan struct{})}
	go c.read()
	return c
}

func (c *rpc) read() {
	sc := bufio.NewScanner(c.r)
	sc.Buffer(make([]byte, 64<<10), 64<<20)
	for sc.Scan() {
		var m message
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			c.fail(fmt.Errorf("invalid message from harness: %w", err))
			return
		}
		if m.Method != "" {
			c.handle(m)
			continue
		}
		var id int64
		if err := json.Unmarshal(m.ID, &id); err != nil {
			c.fail(fmt.Errorf("invalid response id from harness"))
			return
		}
		c.mu.Lock()
		ch := c.pending[id]
		delete(c.pending, id)
		c.mu.Unlock()
		if ch != nil {
			ch <- m
		}
	}
	c.fail(errors.Join(errClosed, sc.Err()))
}

func (c *rpc) fail(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	select {
	case <-c.done:
	default:
		c.err = err
		close(c.done)
	}
}

func (c *rpc) write(m message) error {
	m.JSONRPC = c.version
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_, err = c.w.Write(append(b, '\n'))
	return err
}

// call sends a request and decodes its result into out (if not nil).
func (c *rpc) call(ctx context.Context, method string, params, out any) error {
	raw, err := json.Marshal(params)
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.next++
	id := c.next
	ch := make(chan message, 1)
	c.pending[id] = ch
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}()
	if err := c.write(message{ID: json.RawMessage(fmt.Sprint(id)), Method: method, Params: raw}); err != nil {
		return err
	}
	select {
	case m := <-ch:
		if m.Error != nil {
			return fmt.Errorf("%s: %s (code %d)", method, m.Error.Message, m.Error.Code)
		}
		if out != nil {
			return json.Unmarshal(m.Result, out)
		}
		return nil
	case <-c.done:
		return fmt.Errorf("%s: %w", method, c.err)
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *rpc) notify(method string, params any) error {
	raw, err := json.Marshal(params)
	if err != nil {
		return err
	}
	return c.write(message{Method: method, Params: raw})
}

func (c *rpc) reply(id json.RawMessage, result any, rerr *rpcError) error {
	m := message{ID: id, Error: rerr}
	if rerr == nil {
		raw, err := json.Marshal(result)
		if err != nil {
			return err
		}
		m.Result = raw
	}
	return c.write(m)
}

// unsupported rejects a request from the harness that Reach does not serve.
func (c *rpc) unsupported(m message) {
	c.reply(m.ID, nil, &rpcError{Code: -32601, Message: "method not supported by Reach"})
}

// inbox is an unbounded queue of harness messages, so the reader goroutine
// never blocks and no message is dropped.
type inbox struct {
	mu     sync.Mutex
	items  []message
	signal chan struct{}
}

func newInbox() *inbox { return &inbox{signal: make(chan struct{}, 1)} }

func (q *inbox) push(m message) {
	q.mu.Lock()
	q.items = append(q.items, m)
	q.mu.Unlock()
	select {
	case q.signal <- struct{}{}:
	default:
	}
}

// pop returns the oldest message, if any.
func (q *inbox) pop() (message, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.items) == 0 {
		return message{}, false
	}
	m := q.items[0]
	q.items = q.items[1:]
	return m, true
}

// reset drops queued messages.
func (q *inbox) reset() {
	q.mu.Lock()
	q.items = nil
	q.mu.Unlock()
}
