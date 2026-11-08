package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/takezoh/syntoria-reach/src/internal/agent/agenttest"
	"github.com/takezoh/syntoria-reach/src/internal/core"
)

func fakeAgent(t *testing.T, protocol string) core.Agent {
	t.Helper()
	cmd, env := agenttest.Agent(t, protocol, "TestFakeHarness")
	return core.Agent{ID: protocol, Role: "test", Command: cmd, Protocol: protocol, Env: env}
}

func start(t *testing.T, a core.Agent) (*Runtime, string) {
	t.Helper()
	logs := filepath.Join(t.TempDir(), "agents")
	m := New(logs)
	if err := m.Start(context.Background(), "ses_1", t.TempDir(), a); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Stop("ses_1") })
	return m, logs
}

// settle waits until the agent of session id no longer works.
func settle(t *testing.T, m *Runtime, id string) *core.AgentStatus {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		st, err := m.Status(id)
		if err != nil {
			t.Fatal(err)
		}
		if st.State != core.AgentWorking {
			return st
		}
		if time.Now().After(deadline) {
			t.Fatalf("still working: %+v", st)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func code(err error) string {
	var e *core.Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

func TestSessions(t *testing.T) {
	cases := map[string]string{
		// Approvals and permission requests are refused: the agent's own
		// settings say what it may do.
		core.ProtocolCodex:  "echo hi (approval=rejected)",
		core.ProtocolACP:    "echo hi (permission=cancelled)",
		core.ProtocolClaude: "echo hi",
	}
	for protocol, want := range cases {
		t.Run(protocol, func(t *testing.T) {
			m, logs := start(t, fakeAgent(t, protocol))
			if st := settle(t, m, "ses_1"); st.State != core.AgentIdle || st.Reply != "" {
				t.Fatalf("before a prompt: %+v", st)
			}
			if err := m.Prompt("ses_1", "hi"); err != nil {
				t.Fatal(err)
			}
			if st := settle(t, m, "ses_1"); st.State != core.AgentIdle || st.Reply != want || st.ReplyTruncated {
				t.Fatalf("reply: %+v", st)
			}

			// A turn in progress takes no prompt; Stop interrupts it.
			if err := m.Prompt("ses_1", "hang"); err != nil {
				t.Fatal(err)
			}
			if st, _ := m.Status("ses_1"); st.State != core.AgentWorking || st.Reply != "" {
				t.Fatalf("during a turn: %+v", st)
			}
			if err := m.Prompt("ses_1", "again"); code(err) != core.CodeInvalidState {
				t.Fatalf("prompt during a turn: %v", err)
			}
			if err := m.Stop("ses_1"); err != nil {
				t.Fatal(err)
			}
			if _, err := m.Status("ses_1"); !errors.Is(err, core.ErrNotFound) {
				t.Fatalf("status after stop: %v", err)
			}
			if err := m.Prompt("ses_1", "late"); !errors.Is(err, core.ErrNotFound) {
				t.Fatalf("prompt after stop: %v", err)
			}
			if err := m.Stop("ses_1"); !errors.Is(err, core.ErrNotFound) {
				t.Fatalf("second stop: %v", err)
			}
			if entries, _ := os.ReadDir(logs); len(entries) != 0 {
				t.Fatalf("the log of a stopped session stayed: %v", entries)
			}
		})
	}
}

// A turn that fails leaves the failure as its reply.
func TestClaudeFailedTurn(t *testing.T) {
	m, _ := start(t, fakeAgent(t, core.ProtocolClaude))
	settle(t, m, "ses_1")
	if err := m.Prompt("ses_1", "fail"); err != nil {
		t.Fatal(err)
	}
	if st := settle(t, m, "ses_1"); st.State != core.AgentIdle || st.Reply != "boom" {
		t.Fatalf("failed turn: %+v", st)
	}
}

// An agent that quits is exited, and takes no prompt.
func TestAgentCrash(t *testing.T) {
	for _, protocol := range []string{core.ProtocolCodex, core.ProtocolClaude, core.ProtocolACP} {
		t.Run(protocol, func(t *testing.T) {
			m, _ := start(t, fakeAgent(t, protocol))
			settle(t, m, "ses_1")
			if err := m.Prompt("ses_1", "crash"); err != nil {
				t.Fatal(err)
			}
			if st := settle(t, m, "ses_1"); st.State != core.AgentExited {
				t.Fatalf("after the crash: %+v", st)
			}
			if err := m.Prompt("ses_1", "late"); code(err) != core.CodeInvalidState {
				t.Fatalf("prompt after the crash: %v", err)
			}
		})
	}
}

// An agent that does not speak its protocol does not start, and leaves no
// session.
func TestStartFailure(t *testing.T) {
	a := fakeAgent(t, core.ProtocolACP)
	a.Env = map[string]string{"REACH_FAKE_AGENT": ""} // exits without speaking ACP
	m := New(t.TempDir())
	if err := m.Start(context.Background(), "ses_1", t.TempDir(), a); code(err) != core.CodeHarnessFailed {
		t.Fatalf("start: %v", err)
	}
	if _, err := m.Status("ses_1"); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("failed session: %v", err)
	}
	a.Command = []string{"/nonexistent/agent"}
	if err := m.Start(context.Background(), "ses_2", t.TempDir(), a); code(err) != core.CodeHarnessFailed {
		t.Fatalf("missing command: %v", err)
	}
}

// Start returns after StartWait with an agent still starting, which works
// until it is up.
func TestSlowStart(t *testing.T) {
	a := fakeAgent(t, core.ProtocolCodex)
	a.Env["REACH_FAKE_SLOW_START"] = "1"
	m := New(t.TempDir())
	m.StartWait = 100 * time.Millisecond
	t.Cleanup(func() { m.Stop("ses_1") })
	begun := time.Now()
	if err := m.Start(context.Background(), "ses_1", t.TempDir(), a); err != nil || time.Since(begun) > 900*time.Millisecond {
		t.Fatalf("start: %v after %v", err, time.Since(begun))
	}
	if st, _ := m.Status("ses_1"); st.State != core.AgentWorking {
		t.Fatalf("while starting: %+v", st)
	}
	if err := m.Prompt("ses_1", "hi"); code(err) != core.CodeInvalidState {
		t.Fatalf("prompt while starting: %v", err)
	}
	if st := settle(t, m, "ses_1"); st.State != core.AgentIdle {
		t.Fatalf("once up: %+v", st)
	}
}

func TestCut(t *testing.T) {
	for _, c := range []struct {
		in        string
		n         int
		out       string
		truncated bool
	}{
		{"hello", 10, "hello", false},
		{"hello", 5, "hello", false},
		{"hello", 4, "hell", true},
		{"日本語", 4, "日", true}, // never in the middle of a character
		{strings.Repeat("a", MaxReply+1), MaxReply, strings.Repeat("a", MaxReply), true},
	} {
		if out, truncated := cut(c.in, c.n); out != c.out || truncated != c.truncated {
			t.Errorf("cut(%.10q, %d) = %q, %v", c.in, c.n, out, truncated)
		}
	}
}
