package agent

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/takezoh/syntoria-reach/src/internal/agent/agenttest"
	"github.com/takezoh/syntoria-reach/src/internal/config"
	"github.com/takezoh/syntoria-reach/src/internal/store"
)

func newManager(t *testing.T) (*Manager, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "reach.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return New(st, filepath.Join(dir, "agents")), st
}

func fakeAgent(t *testing.T, protocol string) config.Agent {
	t.Helper()
	cmd, env := agenttest.Agent(t, protocol, "TestFakeHarness")
	a := config.Agent{ID: protocol, Protocol: protocol, Command: cmd, Env: env}
	switch protocol {
	case config.ProtocolCodex:
		a.Sandbox = "workspace-write"
	case config.ProtocolACP:
		a.AllowPermissions = true
	}
	return a
}

func start(t *testing.T, m *Manager, a config.Agent, resume string) *store.AgentSession {
	t.Helper()
	sess, err := m.Start(context.Background(), StartSpec{ClientID: "c", WorkspaceID: "ws", Dir: t.TempDir(), Agent: a, Resume: resume})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Stop(sess.ID) })
	return sess
}

// waitTurn waits until the session leaves the running state and returns its events.
func waitTurn(t *testing.T, st *store.Store, id string) (*store.AgentSession, []store.AgentEvent) {
	t.Helper()
	for range 200 {
		s, err := st.GetAgentSession("c", id)
		if err != nil {
			t.Fatal(err)
		}
		if s.State != store.SessionRunning {
			events, err := st.ListAgentEvents(id, 0, 100)
			if err != nil {
				t.Fatal(err)
			}
			return s, events
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("turn did not finish")
	return nil, nil
}

func summary(events []store.AgentEvent) string {
	var parts []string
	for _, e := range events {
		parts = append(parts, e.Type+":"+e.Text)
	}
	return strings.Join(parts, " | ")
}

func TestSessions(t *testing.T) {
	cases := map[string]string{
		config.ProtocolCodex:  "message:echo hi (sandbox=workspace-write approval=rejected) | turn_completed:completed",
		config.ProtocolACP:    "message:echo hi (permission=selected yes) | turn_completed:end_turn",
		config.ProtocolClaude: "message:echo hi (resumed=false) | turn_completed:success",
	}
	for protocol, want := range cases {
		t.Run(protocol, func(t *testing.T) {
			m, st := newManager(t)
			sess := start(t, m, fakeAgent(t, protocol), "")
			if sess.State != store.SessionIdle || sess.ProtocolSessionID == "" {
				t.Fatalf("session = %+v", sess)
			}
			turn, err := m.Prompt(sess.ID, "hi")
			if err != nil || turn != 1 {
				t.Fatalf("turn=%d err=%v", turn, err)
			}
			got, events := waitTurn(t, st, sess.ID)
			if got.State != store.SessionIdle || got.Turns != 1 || summary(events) != want {
				t.Fatalf("state=%s turns=%d events=%s", got.State, got.Turns, summary(events))
			}

			// A second prompt while a turn hangs is rejected; Stop interrupts it.
			if _, err := m.Prompt(sess.ID, "hang"); err != nil {
				t.Fatal(err)
			}
			if _, err := m.Prompt(sess.ID, "again"); err != ErrBusy {
				t.Fatalf("concurrent prompt err = %v", err)
			}
			if err := m.Stop(sess.ID); err != nil {
				t.Fatal(err)
			}
			got, _ = st.GetAgentSession("c", sess.ID)
			if got.State != store.SessionClosed {
				t.Fatalf("after stop: %s", got.State)
			}
			if _, err := m.Prompt(sess.ID, "late"); err != ErrNotActive {
				t.Fatalf("prompt after stop err = %v", err)
			}
		})
	}
}

func TestResume(t *testing.T) {
	const claudeID = "0b5e2c7e-4c1d-4f6a-9c3b-2a1d0e9f8b7a"
	cases := map[string]struct {
		protocol, resume string
		env              map[string]string
	}{
		"codex":              {config.ProtocolCodex, "previous-thread", nil},
		"acp session/load":   {config.ProtocolACP, "previous-session", nil},
		"acp session/resume": {config.ProtocolACP, "previous-session", map[string]string{"REACH_FAKE_ACP_RESUME": "1"}},
		"claude":             {config.ProtocolClaude, claudeID, nil},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			m, st := newManager(t)
			a := fakeAgent(t, c.protocol)
			for k, v := range c.env {
				a.Env[k] = v
			}
			sess := start(t, m, a, c.resume)
			if sess.ProtocolSessionID != c.resume {
				t.Fatalf("protocol session = %q", sess.ProtocolSessionID)
			}
			// History replayed by session/load is not recorded as events.
			if events, _ := st.ListAgentEvents(sess.ID, 0, 10); len(events) != 0 {
				t.Fatalf("replayed history recorded: %s", summary(events))
			}
		})
	}
	m, _ := newManager(t)
	if _, err := m.Start(context.Background(), StartSpec{ClientID: "c", WorkspaceID: "ws", Dir: t.TempDir(),
		Agent: fakeAgent(t, config.ProtocolClaude), Resume: "not-a-uuid"}); err == nil {
		t.Fatal("invalid Claude session id accepted")
	}
}

func TestClaudeFailedTurn(t *testing.T) {
	m, st := newManager(t)
	sess := start(t, m, fakeAgent(t, config.ProtocolClaude), "")
	if _, err := m.Prompt(sess.ID, "fail"); err != nil {
		t.Fatal(err)
	}
	got, events := waitTurn(t, st, sess.ID)
	if got.State != store.SessionIdle || summary(events) != "error:boom | turn_completed:error_during_execution" {
		t.Fatalf("state=%s events=%s", got.State, summary(events))
	}
}

func TestHarnessCrashFailsSession(t *testing.T) {
	for _, protocol := range []string{config.ProtocolCodex, config.ProtocolClaude, config.ProtocolACP} {
		t.Run(protocol, func(t *testing.T) {
			m, st := newManager(t)
			sess := start(t, m, fakeAgent(t, protocol), "")
			if _, err := m.Prompt(sess.ID, "crash"); err != nil {
				t.Fatal(err)
			}
			for range 200 {
				if got, _ := st.GetAgentSession("c", sess.ID); got.State == store.SessionFailed {
					return
				}
				time.Sleep(20 * time.Millisecond)
			}
			got, _ := st.GetAgentSession("c", sess.ID)
			t.Fatalf("state = %s", got.State)
		})
	}
}

func TestStartFailure(t *testing.T) {
	m, _ := newManager(t)
	a := fakeAgent(t, config.ProtocolACP)
	a.Env = map[string]string{"REACH_FAKE_AGENT": ""} // exits without speaking ACP
	if _, err := m.Start(context.Background(), StartSpec{ClientID: "c", WorkspaceID: "ws", Dir: t.TempDir(), Agent: a}); err == nil {
		t.Fatal("start succeeded")
	}
}
