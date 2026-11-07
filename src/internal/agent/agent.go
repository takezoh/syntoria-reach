// Package agent runs agent harnesses as sessions: Codex (app-server), Claude
// Code (stream-json) and ACP agents such as DSH. The protocol clients and the
// process handling are ported from DaVinci's agent adapters and execution
// package, without DaVinci's prompts and output contracts. Reach starts the
// harness, opens a protocol session, delivers prompts one turn at a time and
// records normalized events. The harness sandboxes its own tools; Reach does
// not interpret what the agent produces.
package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/takezoh/syntoria-reach/src/internal/command"
	"github.com/takezoh/syntoria-reach/src/internal/config"
	"github.com/takezoh/syntoria-reach/src/internal/store"
)

// Event types recorded for a session.
const (
	EventMessage       = "message"        // text the agent sent to the user
	EventError         = "error"          // an error reported by the harness or Reach
	EventTurnCompleted = "turn_completed" // text is the stop reason / turn status
)

var (
	// ErrNotActive: the session is not running in this daemon.
	ErrNotActive = errors.New("agent session is not active")
	// ErrBusy: a turn is already in progress.
	ErrBusy = errors.New("a turn is already in progress")
)

// harness is one protocol connection to a running harness process.
type harness interface {
	open(ctx context.Context, cwd, resume string) (protocolSessionID string, err error)
	prompt(ctx context.Context, text string, emit func(typ, text string)) (stopReason string, err error)
	// close ends any turn and the protocol session and closes the input.
	close(ctx context.Context) error
	// done is closed when the protocol reader has stopped consuming stdout.
	done() <-chan struct{}
}

const (
	openTimeout  = 60 * time.Second
	closeTimeout = 10 * time.Second
	stderrLimit  = 16 << 20 // per session harness log
)

// Manager owns the running sessions of this daemon.
type Manager struct {
	store    *store.Store
	logDir   string
	mu       sync.Mutex
	sessions map[string]*session
}

type session struct {
	id      string
	proc    *process
	h       harness
	exited  chan struct{}
	mu      sync.Mutex
	busy    bool
	turn    int
	stopped bool
}

// New returns a manager that keeps harness stderr logs under logDir.
func New(st *store.Store, logDir string) *Manager {
	return &Manager{store: st, logDir: logDir, sessions: map[string]*session{}}
}

// StartSpec describes a session to start.
type StartSpec struct {
	ClientID    string
	WorkspaceID string
	Dir         string
	Agent       config.Agent
	Resume      string // protocol session ID to resume, if any
	Model       string // Codex and Claude
	Effort      string // Codex and Claude
}

// Start launches the harness and opens a protocol session within a minute.
// ctx should not be tied to the caller's connection. On failure the session
// is recorded as failed and the error is returned.
func (m *Manager) Start(ctx context.Context, s StartSpec) (*store.AgentSession, error) {
	id := "as_" + randomID()
	if err := m.store.CreateAgentSession(&store.AgentSession{ID: id, ClientID: s.ClientID, WorkspaceID: s.WorkspaceID, AgentID: s.Agent.ID}); err != nil {
		return nil, err
	}
	sess, protoID, err := m.launch(ctx, id, s)
	if err != nil {
		m.store.UpdateAgentSession(id, store.AgentSessionUpdate{State: store.SessionFailed, ErrorMessage: err.Error()})
		return nil, err
	}
	if err := m.store.UpdateAgentSession(id, store.AgentSessionUpdate{State: store.SessionIdle, ProtocolSessionID: protoID}); err != nil {
		m.closeSession(sess)
		return nil, err
	}
	m.mu.Lock()
	m.sessions[id] = sess
	m.mu.Unlock()
	go m.watch(sess)
	return m.store.GetAgentSession(s.ClientID, id)
}

func (m *Manager) launch(ctx context.Context, id string, s StartSpec) (*session, string, error) {
	if err := os.MkdirAll(m.logDir, 0o700); err != nil {
		return nil, "", err
	}
	argv := append([]string{}, s.Agent.Command...)
	claudeSession := s.Resume
	if s.Agent.Protocol == config.ProtocolClaude {
		if claudeSession == "" {
			claudeSession = newClaudeSessionID()
		} else if !validClaudeSessionID(claudeSession) {
			return nil, "", fmt.Errorf("invalid Claude session id %q", claudeSession)
		}
		argv = append(argv, claudeArgs(claudeSession, s.Resume != "", s.Model, s.Effort)...)
	}
	proc, err := startProcess(argv, s.Dir, command.Env(s.Agent.Env), filepath.Join(m.logDir, id+".stderr"))
	if err != nil {
		return nil, "", fmt.Errorf("start harness: %w", err)
	}
	sess := &session{id: id, proc: proc, exited: make(chan struct{})}
	switch s.Agent.Protocol {
	case config.ProtocolCodex:
		sess.h = newCodex(proc.stdin, proc.stdout, s.Agent.Sandbox, s.Model, s.Effort)
	case config.ProtocolClaude:
		sess.h = newClaude(proc.stdin, proc.stdout, claudeSession)
	default:
		sess.h = newACP(proc.stdin, proc.stdout, s.Agent.AllowPermissions)
	}
	go func() {
		proc.wait(sess.h.done())
		close(sess.exited)
	}()
	octx, cancel := context.WithTimeout(ctx, openTimeout)
	defer cancel()
	protoID, err := sess.h.open(octx, s.Dir, s.Resume)
	if err != nil {
		m.closeSession(sess)
		return nil, "", fmt.Errorf("open session: %w", err)
	}
	return sess, protoID, nil
}

// watch marks the session failed if the harness exits without Stop.
func (m *Manager) watch(sess *session) {
	<-sess.exited
	m.mu.Lock()
	delete(m.sessions, sess.id)
	m.mu.Unlock()
	sess.mu.Lock()
	stopped, turn := sess.stopped, sess.turn
	sess.mu.Unlock()
	if !stopped {
		m.store.AppendAgentEvent(sess.id, turn, EventError, "harness exited")
		m.store.UpdateAgentSession(sess.id, store.AgentSessionUpdate{State: store.SessionFailed, ErrorMessage: "harness exited"})
	}
}

func (m *Manager) active(id string) (*session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	sess, ok := m.sessions[id]
	if !ok {
		return nil, ErrNotActive
	}
	return sess, nil
}

// Prompt starts a turn and returns its number without waiting for the agent.
// The turn's output is recorded as events.
func (m *Manager) Prompt(id, text string) (int, error) {
	sess, err := m.active(id)
	if err != nil {
		return 0, err
	}
	sess.mu.Lock()
	if sess.busy || sess.stopped {
		sess.mu.Unlock()
		return 0, ErrBusy
	}
	sess.busy = true
	sess.turn++
	turn := sess.turn
	sess.mu.Unlock()
	if err := m.store.UpdateAgentSession(id, store.AgentSessionUpdate{State: store.SessionRunning, AddTurn: true}); err != nil {
		sess.mu.Lock()
		sess.busy = false
		sess.mu.Unlock()
		return 0, err
	}

	go func() {
		emit := func(typ, text string) {
			if err := m.store.AppendAgentEvent(id, turn, typ, text); err != nil {
				slog.Error("record agent event", "session", id, "err", err)
			}
		}
		stop, err := sess.h.prompt(context.Background(), text, emit)
		if err != nil {
			emit(EventError, err.Error())
		} else {
			emit(EventTurnCompleted, stop)
		}
		sess.mu.Lock()
		sess.busy = false
		sess.mu.Unlock()
		select {
		case <-sess.h.done():
			// watch records the failure.
		default:
			m.store.UpdateAgentSession(id, store.AgentSessionUpdate{State: store.SessionIdle})
		}
	}()
	return turn, nil
}

// Stop ends any turn and the protocol session, ends the harness process and
// marks the session closed. Stopping an inactive session returns ErrNotActive.
func (m *Manager) Stop(id string) error {
	sess, err := m.active(id)
	if err != nil {
		return err
	}
	sess.mu.Lock()
	sess.stopped = true
	sess.mu.Unlock()
	m.closeSession(sess)
	return m.store.UpdateAgentSession(id, store.AgentSessionUpdate{State: store.SessionClosed})
}

// closeSession performs the protocol close, then kills and reaps the harness.
func (m *Manager) closeSession(sess *session) {
	ctx, cancel := context.WithTimeout(context.Background(), closeTimeout)
	defer cancel()
	if err := sess.proc.close(ctx, sess.h); err != nil {
		slog.Warn("harness close", "session", sess.id, "err", err)
	}
	<-sess.exited
}

func randomID() string {
	b := make([]byte, 12)
	rand.Read(b)
	return hex.EncodeToString(b)
}
