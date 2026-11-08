// Package agent runs agents as processes of the daemon, for local
// workspaces: Codex (app-server), Claude Code (stream-json) and ACP agents
// such as DSH. The protocol clients and the process handling are ported
// from DaVinci's agent adapters and execution package, without DaVinci's
// prompts and output contracts. A runtime keeps its sessions in memory: the
// agents are children of the daemon and end with it. A session runs one
// turn at a time and keeps the agent's last message of the turn. The agent
// sandboxes its own tools.
package agent

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/takezoh/syntoria-reach/src/internal/command"
	"github.com/takezoh/syntoria-reach/src/internal/core"
)

// What a protocol client reports of a turn.
const (
	eventMessage = "message" // a message of the agent
	eventError   = "error"   // a failure the agent or the protocol reported
)

// harness is one protocol connection to a running agent process.
type harness interface {
	open(ctx context.Context, cwd string) error
	prompt(ctx context.Context, text string, emit func(typ, text string)) (stopReason string, err error)
	// close ends any turn and the protocol session and closes the input.
	close(ctx context.Context) error
	// done is closed when the protocol reader has stopped consuming stdout.
	done() <-chan struct{}
}

const (
	openTimeout  = 2 * time.Minute
	closeTimeout = 10 * time.Second
	stderrLimit  = 16 << 20 // per session log
	// MaxReply is the most of a reply a session keeps, in bytes.
	MaxReply = 64 << 10
)

// Runtime owns the agent processes of this daemon, by session ID.
type Runtime struct {
	logDir string
	// StartWait is how long Start waits for the agent to open its session
	// before it returns with the agent still starting; zero means 15 s,
	// which keeps a start within the remote gateway's 30 s.
	StartWait time.Duration

	mu       sync.Mutex
	sessions map[string]*session
}

type session struct {
	id     string
	proc   *process
	h      harness
	exited chan struct{} // closed once the agent process ended
	log    string

	mu        sync.Mutex
	opening   bool          // the protocol session is being opened
	ongoing   chan struct{} // closed when the turn in progress ended; nil if none
	reply     string
	truncated bool
}

// New returns a runtime that keeps each session's agent stderr in a file
// under logDir until the session is stopped.
func New(logDir string) *Runtime {
	return &Runtime{logDir: logDir, sessions: map[string]*session{}}
}

// Start starts agent a in dir as session id. It returns once the agent
// opened its protocol session, or after StartWait with the agent still
// starting; an agent that fails to start or open is harness_unavailable,
// and leaves no session. ctx should not be tied to the caller's connection.
func (m *Runtime) Start(ctx context.Context, id, dir string, a core.Agent) error {
	if err := os.MkdirAll(m.logDir, 0o700); err != nil {
		return err
	}
	argv := slices.Clone(a.Command)
	claudeSession := ""
	if a.Protocol == core.ProtocolClaude {
		claudeSession = newClaudeSessionID()
		argv = append(argv, claudeArgs(claudeSession)...)
	}
	log := filepath.Join(m.logDir, id+".stderr")
	proc, err := startProcess(argv, dir, command.Env(a.Env), log)
	if err != nil {
		return core.Errorf(core.CodeHarnessFailed, "cannot start agent %q: %v", a.ID, err)
	}
	sess := &session{id: id, proc: proc, exited: make(chan struct{}), log: log, opening: true}
	switch a.Protocol {
	case core.ProtocolCodex:
		sess.h = newCodex(proc.stdin, proc.stdout)
	case core.ProtocolClaude:
		sess.h = newClaude(proc.stdin, proc.stdout, claudeSession)
	default:
		sess.h = newACP(proc.stdin, proc.stdout)
	}
	go func() {
		proc.wait(sess.h.done())
		close(sess.exited)
	}()
	m.mu.Lock()
	m.sessions[id] = sess
	m.mu.Unlock()

	opened := make(chan error, 1)
	go func() {
		octx, cancel := context.WithTimeout(context.WithoutCancel(ctx), openTimeout)
		defer cancel()
		err := sess.h.open(octx, dir)
		if err != nil {
			// The agent is gone; the session reads exited until stopped.
			m.closeSession(sess)
		}
		sess.mu.Lock()
		sess.opening = false
		sess.mu.Unlock()
		opened <- err
	}()
	wait := m.StartWait
	if wait <= 0 {
		wait = 15 * time.Second
	}
	select {
	case err := <-opened:
		if err != nil {
			m.mu.Lock()
			delete(m.sessions, id)
			m.mu.Unlock()
			return core.Errorf(core.CodeHarnessFailed, "agent %q did not start: %v", a.ID, err)
		}
	case <-time.After(wait):
	}
	return nil
}

// Status tells what the agent of session id does and, once it waits for a
// prompt, its last message of the turn; core.ErrNotFound if the runtime has
// no such session.
func (m *Runtime) Status(id string) (*core.AgentStatus, error) {
	sess, err := m.get(id)
	if err != nil {
		return nil, err
	}
	st := &core.AgentStatus{AgentSession: core.AgentSession{ID: id}}
	select {
	case <-sess.exited:
		st.State = core.AgentExited
		return st, nil
	default:
	}
	sess.mu.Lock()
	defer sess.mu.Unlock()
	if sess.opening || sess.ongoing != nil {
		st.State = core.AgentWorking
		return st, nil
	}
	st.State, st.Reply, st.ReplyTruncated = core.AgentIdle, sess.reply, sess.truncated
	return st, nil
}

// Prompt starts a turn with text and returns without waiting for the
// agent. The agent must wait for a prompt; otherwise it is invalid_state.
// Once the turn ends, the session keeps the agent's last message of it, or
// the failure that ended it.
func (m *Runtime) Prompt(id, text string) error {
	sess, err := m.get(id)
	if err != nil {
		return err
	}
	select {
	case <-sess.exited:
		return core.Errorf(core.CodeInvalidState, "the agent of session %q is no longer running", id)
	default:
	}
	sess.mu.Lock()
	if sess.opening || sess.ongoing != nil {
		sess.mu.Unlock()
		return core.Errorf(core.CodeInvalidState, "the agent is not waiting for a prompt: read_agent_output until state is idle")
	}
	ongoing := make(chan struct{})
	sess.ongoing, sess.reply, sess.truncated = ongoing, "", false
	sess.mu.Unlock()

	go func() {
		var mu sync.Mutex
		var message, failure string
		emit := func(typ, text string) {
			mu.Lock()
			defer mu.Unlock()
			switch typ {
			case eventMessage:
				message = text
			case eventError:
				failure = text
			}
		}
		_, err := sess.h.prompt(context.Background(), text, emit)
		mu.Lock()
		reply := message
		if reply == "" {
			reply = failure
		}
		if reply == "" && err != nil {
			reply = err.Error()
		}
		mu.Unlock()
		reply, truncated := cut(reply, MaxReply)
		sess.mu.Lock()
		sess.reply, sess.truncated, sess.ongoing = reply, truncated, nil
		sess.mu.Unlock()
		close(ongoing)
	}()
	return nil
}

// Stop ends any turn and the protocol session, ends the agent process and
// forgets the session; core.ErrNotFound if the runtime has no such session.
func (m *Runtime) Stop(id string) error {
	m.mu.Lock()
	sess, ok := m.sessions[id]
	delete(m.sessions, id)
	m.mu.Unlock()
	if !ok {
		return core.ErrNotFound
	}
	m.closeSession(sess)
	sess.mu.Lock()
	ongoing := sess.ongoing
	sess.mu.Unlock()
	if ongoing != nil {
		<-ongoing
	}
	if err := os.Remove(sess.log); err != nil && !os.IsNotExist(err) {
		slog.Warn("cannot remove an agent log", "path", sess.log, "err", err)
	}
	return nil
}

// Close stops every session, as the daemon stops.
func (m *Runtime) Close() {
	m.mu.Lock()
	ids := make([]string, 0, len(m.sessions))
	for id := range m.sessions {
		ids = append(ids, id)
	}
	m.mu.Unlock()
	for _, id := range ids {
		m.Stop(id)
	}
}

func (m *Runtime) get(id string) (*session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	sess, ok := m.sessions[id]
	if !ok {
		return nil, core.ErrNotFound
	}
	return sess, nil
}

// closeSession performs the protocol close, then kills and reaps the agent.
func (m *Runtime) closeSession(sess *session) {
	ctx, cancel := context.WithTimeout(context.Background(), closeTimeout)
	defer cancel()
	if err := sess.proc.close(ctx, sess.h); err != nil {
		slog.Warn("agent close", "session", sess.id, "err", err)
	}
	<-sess.exited
}

// cut returns the first n bytes of s, at a character boundary, and whether
// anything was left out.
func cut(s string, n int) (string, bool) {
	if len(s) <= n {
		return s, false
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n], true
}
