package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
)

// claude drives Claude Code's stream-json interface (`claude --print
// --input-format stream-json --output-format stream-json`). Ported from
// DaVinci's claude.Client without its DaVinci-specific checks. Claude Code's
// own model, effort, permission mode and sandbox settings apply; they are
// part of the configured command.
type claude struct {
	input    io.WriteCloser
	output   io.Reader
	expected string // session UUID passed with --session-id

	mu       sync.Mutex
	session  string
	readErr  error
	writeMu  sync.Mutex
	messages chan claudeMessage
	finished chan struct{}
	stop     chan struct{}
	stopOnce sync.Once
}

type claudeMessage struct {
	Type      string `json:"type"`
	Subtype   string `json:"subtype"`
	SessionID string `json:"session_id"`
	Result    string `json:"result"`
	IsError   bool   `json:"is_error"`
	RequestID string `json:"request_id"`
	Response  struct {
		Subtype   string `json:"subtype"`
		RequestID string `json:"request_id"`
		Error     string `json:"error"`
	} `json:"response"`
}

const claudeInitializeID = "reach-initialize"

// claudeArgs are the flags Reach adds to a configured Claude command.
func claudeArgs(sessionID string) []string {
	return []string{"--print", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose", "--session-id", sessionID}
}

// newClaudeSessionID returns a random lowercase UUID v4.
func newClaudeSessionID() string {
	var raw [16]byte
	rand.Read(raw[:])
	raw[6] = (raw[6] & 0x0f) | 0x40
	raw[8] = (raw[8] & 0x3f) | 0x80
	e := hex.EncodeToString(raw[:])
	return e[0:8] + "-" + e[8:12] + "-" + e[12:16] + "-" + e[16:20] + "-" + e[20:32]
}

func newClaude(w io.WriteCloser, r io.Reader, sessionID string) *claude {
	c := &claude{input: w, output: r, expected: sessionID,
		messages: make(chan claudeMessage, 32), finished: make(chan struct{}), stop: make(chan struct{})}
	go c.read()
	return c
}

func (c *claude) done() <-chan struct{} { return c.finished }

func (c *claude) read() {
	defer close(c.finished)
	dec := json.NewDecoder(c.output)
	for {
		var m claudeMessage
		if err := dec.Decode(&m); err != nil {
			c.mu.Lock()
			c.readErr = err
			c.mu.Unlock()
			return
		}
		if m.Type == "" {
			c.mu.Lock()
			c.readErr = errors.New("invalid Claude stream event")
			c.mu.Unlock()
			return
		}
		select {
		case c.messages <- m:
		case <-c.stop:
			return
		}
	}
}

// next returns the next message; messages read before EOF are still
// delivered before the end of the stream is reported.
func (c *claude) next(ctx context.Context) (claudeMessage, error) {
	select {
	case m := <-c.messages:
		return m, nil
	default:
	}
	select {
	case m := <-c.messages:
		return m, nil
	case <-c.finished:
		select {
		case m := <-c.messages:
			return m, nil
		default:
		}
		c.mu.Lock()
		err := c.readErr
		c.mu.Unlock()
		if err == nil || errors.Is(err, io.EOF) {
			return claudeMessage{}, errors.New("Claude Code stream ended")
		}
		return claudeMessage{}, fmt.Errorf("Claude Code stream failed: %w", err)
	case <-ctx.Done():
		return claudeMessage{}, ctx.Err()
	}
}

func (c *claude) write(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_, err = c.input.Write(append(b, '\n'))
	return err
}

// verifyInit checks that Claude runs the session Reach asked for.
func (c *claude) verifyInit(m claudeMessage) error {
	if m.SessionID != c.expected {
		return fmt.Errorf("Claude session identity mismatch: got %q", m.SessionID)
	}
	c.mu.Lock()
	c.session = m.SessionID
	c.mu.Unlock()
	return nil
}

// open initializes the stream; cwd is already the process directory.
func (c *claude) open(ctx context.Context, cwd string) error {
	if err := c.write(map[string]any{
		"type": "control_request", "request_id": claudeInitializeID,
		"request": map[string]string{"subtype": "initialize"},
	}); err != nil {
		return err
	}
	for {
		m, err := c.next(ctx)
		if err != nil {
			return err
		}
		if m.Type == "system" && m.Subtype == "init" {
			if err := c.verifyInit(m); err != nil {
				return err
			}
			continue
		}
		if m.Type != "control_response" {
			continue
		}
		id := m.Response.RequestID
		if id == "" {
			id = m.RequestID
		}
		if id != claudeInitializeID {
			continue
		}
		if m.Response.Subtype != "success" {
			return fmt.Errorf("Claude stream initialization failed: %s", m.Response.Error)
		}
		return nil
	}
}

func (c *claude) prompt(ctx context.Context, text string, emit func(typ, text string)) (string, error) {
	if err := c.write(map[string]any{"type": "user", "message": map[string]any{
		"role": "user", "content": []map[string]string{{"type": "text", "text": text}},
	}}); err != nil {
		return "", err
	}
	for {
		m, err := c.next(ctx)
		if err != nil {
			return "", err
		}
		if m.Type == "system" && m.Subtype == "init" {
			if err := c.verifyInit(m); err != nil {
				return "", err
			}
			continue
		}
		if m.Type != "result" {
			continue
		}
		if m.SessionID != "" && m.SessionID != c.expected {
			return "", errors.New("Claude result belongs to another session")
		}
		if m.Subtype != "success" || m.IsError {
			if m.Result != "" {
				emit(eventError, m.Result)
			}
			return m.Subtype, nil
		}
		if m.Result != "" {
			emit(eventMessage, m.Result)
		}
		return m.Subtype, nil
	}
}

func (c *claude) close(context.Context) error {
	c.stopOnce.Do(func() { close(c.stop) })
	return c.input.Close()
}
