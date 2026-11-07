// Package agent runs agent harnesses as sessions: Codex (app-server), Claude
// Code (stream-json) and ACP agents such as DSH. The protocol clients and the
// process handling are ported from DaVinci's agent adapters and execution
// package, without DaVinci's prompts and output contracts. The manager starts
// the harness, opens a protocol session and delivers prompts one turn at a
// time. It records nothing itself: what happens is reported to the caller
// through core.AgentEvents. The harness sandboxes its own tools.
package agent

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/takezoh/syntoria-reach/src/internal/command"
	"github.com/takezoh/syntoria-reach/src/internal/core"
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

// Runtime owns the running sessions of this daemon.
type Runtime struct {
	logDir   string
	mu       sync.Mutex
	sessions map[string]*session
}

type session struct {
	id      string
	proc    *process
	h       harness
	events  core.AgentEvents
	exited  chan struct{}
	mu      sync.Mutex
	turn    int
	ongoing chan struct{} // closed when the turn in progress was reported; nil if none
	stopped bool          // Stop was called
	gone    bool          // the harness exited
}

// turnDone returns a channel closed once no turn is in progress.
func (s *session) turnDone() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ongoing == nil {
		c := make(chan struct{})
		close(c)
		return c
	}
	return s.ongoing
}

// New returns a runtime that keeps harness stderr logs under logDir.
func New(logDir string) *Runtime {
	return &Runtime{logDir: logDir, sessions: map[string]*session{}}
}

// Start launches the harness for session id and opens a protocol session
// within a minute; it returns the protocol session ID. ctx should not be
// tied to the caller's connection. Once Start returned, what happens in the
// session is reported to events.
func (m *Runtime) Start(ctx context.Context, id string, s core.AgentStart, events core.AgentEvents) (string, error) {
	sess, protoID, err := m.launch(ctx, id, s)
	if err != nil {
		return "", err
	}
	sess.events = events
	m.mu.Lock()
	m.sessions[id] = sess
	m.mu.Unlock()
	go m.watch(sess)
	return protoID, nil
}

func (m *Runtime) launch(ctx context.Context, id string, s core.AgentStart) (*session, string, error) {
	if err := os.MkdirAll(m.logDir, 0o700); err != nil {
		return nil, "", err
	}
	argv := append([]string{}, s.Harness.Command...)
	claudeSession := s.Resume
	if s.Harness.Protocol == core.ProtocolClaude {
		if claudeSession == "" {
			claudeSession = newClaudeSessionID()
		} else if !validClaudeSessionID(claudeSession) {
			return nil, "", fmt.Errorf("invalid Claude session id %q", claudeSession)
		}
		argv = append(argv, claudeArgs(claudeSession, s.Resume != "", s.Model, s.Effort)...)
	}
	proc, err := startProcess(argv, s.Dir, command.Env(s.Harness.Env), filepath.Join(m.logDir, id+".stderr"))
	if err != nil {
		return nil, "", fmt.Errorf("start harness: %w", err)
	}
	sess := &session{id: id, proc: proc, exited: make(chan struct{})}
	switch s.Harness.Protocol {
	case core.ProtocolCodex:
		sess.h = newCodex(proc.stdin, proc.stdout, s.Harness.Sandbox, s.Model, s.Effort)
	case core.ProtocolClaude:
		sess.h = newClaude(proc.stdin, proc.stdout, claudeSession)
	default:
		sess.h = newACP(proc.stdin, proc.stdout, s.Harness.AllowPermissions)
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

// watch reports the exit of a harness that was not stopped, after any turn
// in progress has been reported.
func (m *Runtime) watch(sess *session) {
	<-sess.exited
	m.mu.Lock()
	delete(m.sessions, sess.id)
	m.mu.Unlock()
	sess.mu.Lock()
	sess.gone = true
	sess.mu.Unlock()
	<-sess.turnDone()
	sess.mu.Lock()
	stopped, turn := sess.stopped, sess.turn
	sess.mu.Unlock()
	if !stopped {
		sess.events.Exited(sess.id, turn)
	}
}

func (m *Runtime) active(id string) (*session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	sess, ok := m.sessions[id]
	if !ok {
		return nil, core.ErrSessionInactive
	}
	return sess, nil
}

// Prompt starts a turn and returns its number without waiting for the agent.
// The turn is reported to the session's events: TurnStarted before the
// prompt is delivered, then its events, then TurnEnded unless the harness
// exited.
func (m *Runtime) Prompt(id, text string) (int, error) {
	sess, err := m.active(id)
	if err != nil {
		return 0, err
	}
	sess.mu.Lock()
	switch {
	case sess.stopped || sess.gone:
		sess.mu.Unlock()
		return 0, core.ErrSessionInactive
	case sess.ongoing != nil:
		sess.mu.Unlock()
		return 0, core.ErrSessionBusy
	}
	sess.turn++
	turn := sess.turn
	ongoing := make(chan struct{})
	sess.ongoing = ongoing
	sess.mu.Unlock()
	end := func() {
		sess.mu.Lock()
		sess.ongoing = nil
		sess.mu.Unlock()
		close(ongoing)
	}
	if err := sess.events.TurnStarted(id, turn); err != nil {
		sess.mu.Lock()
		sess.turn--
		sess.mu.Unlock()
		end()
		return 0, err
	}

	go func() {
		defer end()
		emit := func(typ, text string) { sess.events.Event(id, turn, typ, text) }
		stop, err := sess.h.prompt(context.Background(), text, emit)
		if err != nil {
			emit(core.EventError, err.Error())
		} else {
			emit(core.EventTurnCompleted, stop)
		}
		select {
		case <-sess.h.done():
			// watch reports the exit.
		default:
			sess.events.TurnEnded(id, turn)
		}
	}()
	return turn, nil
}

// Stop ends any turn and the protocol session and ends the harness process.
// It returns after the session's last event was reported; nothing is
// reported afterwards. Stopping an inactive session returns
// core.ErrSessionInactive.
func (m *Runtime) Stop(id string) error {
	m.mu.Lock()
	sess, ok := m.sessions[id]
	delete(m.sessions, id)
	m.mu.Unlock()
	if !ok {
		return core.ErrSessionInactive
	}
	sess.mu.Lock()
	sess.stopped = true
	sess.mu.Unlock()
	m.closeSession(sess)
	<-sess.turnDone()
	return nil
}

// closeSession performs the protocol close, then kills and reaps the harness.
func (m *Runtime) closeSession(sess *session) {
	ctx, cancel := context.WithTimeout(context.Background(), closeTimeout)
	defer cancel()
	if err := sess.proc.close(ctx, sess.h); err != nil {
		slog.Warn("harness close", "session", sess.id, "err", err)
	}
	<-sess.exited
}
