package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"
)

// codex speaks the Codex app-server protocol, following DaVinci's Codex
// client. Sandboxing, model and effort are Codex's own, set in its command
// or configuration; approvals are disabled, so Codex never asks Reach.
type codex struct {
	rpc   *rpc
	queue *inbox

	mu           sync.Mutex
	thread, turn string
	prompting    bool
}

func newCodex(w io.WriteCloser, r io.Reader) *codex {
	c := &codex{queue: newInbox()}
	c.rpc = newRPC(w, r, "", c.handle)
	return c
}

func (c *codex) done() <-chan struct{} { return c.rpc.done }

func (c *codex) handle(m message) {
	if len(m.ID) > 0 {
		// Requests (approvals, auth, user input) are not served.
		c.rpc.unsupported(m)
		return
	}
	if m.Method != "item/completed" && m.Method != "turn/completed" && m.Method != "error" {
		return
	}
	c.mu.Lock()
	active := c.prompting
	c.mu.Unlock()
	if active {
		c.queue.push(m)
	}
}

func (c *codex) open(ctx context.Context, cwd string) error {
	if err := c.rpc.call(ctx, "initialize", map[string]any{
		"clientInfo": map[string]string{"name": "syntoria-reach", "version": "0"},
	}, nil); err != nil {
		return err
	}
	if err := c.rpc.notify("initialized", nil); err != nil {
		return err
	}
	var res struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	if err := c.rpc.call(ctx, "thread/start", map[string]any{"cwd": cwd, "approvalPolicy": "never"}, &res); err != nil {
		return err
	}
	if res.Thread.ID == "" {
		return errors.New("codex returned no thread id")
	}
	c.mu.Lock()
	c.thread = res.Thread.ID
	c.mu.Unlock()
	return nil
}

type codexNotification struct {
	ThreadID string `json:"threadId"`
	TurnID   string `json:"turnId"`
	Item     struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"item"`
	Turn struct {
		ID     string `json:"id"`
		Status string `json:"status"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	} `json:"turn"`
	Error struct {
		Message string `json:"message"`
	} `json:"error"`
	WillRetry bool `json:"willRetry"`
}

func (c *codex) prompt(ctx context.Context, text string, emit func(typ, text string)) (string, error) {
	c.queue.reset()
	c.mu.Lock()
	c.prompting = true
	thread := c.thread
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.prompting, c.turn = false, ""
		c.mu.Unlock()
	}()

	params := map[string]any{
		"threadId": thread,
		"input":    []map[string]any{{"type": "text", "text": text, "text_elements": []any{}}},
	}
	var res struct {
		Turn struct {
			ID string `json:"id"`
		} `json:"turn"`
	}
	if err := c.rpc.call(ctx, "turn/start", params, &res); err != nil {
		return "", err
	}
	turn := res.Turn.ID
	c.mu.Lock()
	c.turn = turn
	c.mu.Unlock()

	for {
		m, err := c.next(ctx)
		if err != nil {
			return "", err
		}
		var p codexNotification
		if json.Unmarshal(m.Params, &p) != nil || p.ThreadID != thread {
			continue
		}
		switch {
		case m.Method == "item/completed" && p.TurnID == turn && p.Item.Type == "agentMessage":
			emit(eventMessage, p.Item.Text)
		case m.Method == "error" && p.TurnID == turn && !p.WillRetry:
			emit(eventError, p.Error.Message)
		case m.Method == "turn/completed" && p.Turn.ID == turn:
			if p.Turn.Status != "completed" && p.Turn.Error != nil {
				emit(eventError, p.Turn.Error.Message)
			}
			return p.Turn.Status, nil
		}
	}
}

// next waits for the next queued notification. Messages read before the
// harness closed its output are still delivered.
func (c *codex) next(ctx context.Context) (message, error) {
	for {
		if m, ok := c.queue.pop(); ok {
			return m, nil
		}
		select {
		case <-c.queue.signal:
		case <-c.rpc.done:
			if m, ok := c.queue.pop(); ok {
				return m, nil
			}
			return message{}, c.rpc.err
		case <-ctx.Done():
			return message{}, ctx.Err()
		}
	}
}

// close interrupts a running turn, unsubscribes from the thread and closes
// the harness input, as DaVinci's Codex client does.
func (c *codex) close(ctx context.Context) error {
	c.mu.Lock()
	thread, turn := c.thread, c.turn
	c.mu.Unlock()
	var err error
	if turn != "" {
		err = c.rpc.call(ctx, "turn/interrupt", map[string]string{"threadId": thread, "turnId": turn}, nil)
	}
	if thread != "" && err == nil {
		err = c.rpc.call(ctx, "thread/unsubscribe", map[string]string{"threadId": thread}, nil)
	}
	return errors.Join(err, c.rpc.w.Close())
}
