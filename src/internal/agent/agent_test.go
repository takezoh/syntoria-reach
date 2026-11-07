package agent

import (
	"context"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/takezoh/syntoria-reach/src/internal/agent/agenttest"
	"github.com/takezoh/syntoria-reach/src/internal/core"
)

// recorder is a core.AgentEvents that keeps what the runtime reported.
type recorder struct {
	mu       sync.Mutex
	log      []string // "start:1", "message:...", "ended:1", "exited:1"
	changed  chan struct{}
	failNext error // returned by the next TurnStarted
}

func newRecorder() *recorder { return &recorder{changed: make(chan struct{}, 1)} }

func (r *recorder) add(s string) {
	r.mu.Lock()
	r.log = append(r.log, s)
	r.mu.Unlock()
	select {
	case r.changed <- struct{}{}:
	default:
	}
}

func (r *recorder) TurnStarted(_ string, turn int) error {
	r.mu.Lock()
	err := r.failNext
	r.failNext = nil
	r.mu.Unlock()
	if err != nil {
		return err
	}
	r.add("start:" + strconv.Itoa(turn))
	return nil
}
func (r *recorder) Event(_ string, _ int, typ, text string) { r.add(typ + ":" + text) }
func (r *recorder) TurnEnded(_ string, turn int)            { r.add("ended:" + strconv.Itoa(turn)) }
func (r *recorder) Exited(_ string, turn int)               { r.add("exited:" + strconv.Itoa(turn)) }

func (r *recorder) String() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Join(r.log, " | ")
}

// waitFor waits until the log ends with suffix.
func (r *recorder) waitFor(t *testing.T, suffix string) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for !strings.HasSuffix(r.String(), suffix) {
		select {
		case <-r.changed:
		case <-deadline:
			t.Fatalf("log = %q, want suffix %q", r.String(), suffix)
		}
	}
}

func fakeHarness(t *testing.T, protocol string) core.Harness {
	t.Helper()
	cmd, env := agenttest.Agent(t, protocol, "TestFakeHarness")
	h := core.Harness{Protocol: protocol, Command: cmd, Env: env}
	switch protocol {
	case core.ProtocolCodex:
		h.Sandbox = "workspace-write"
	case core.ProtocolACP:
		h.AllowPermissions = true
	}
	return h
}

func start(t *testing.T, h core.Harness, resume string) (*Runtime, *recorder, string) {
	t.Helper()
	m := New(filepath.Join(t.TempDir(), "agents"))
	r := newRecorder()
	proto, err := m.Start(context.Background(), "as_1", core.AgentStart{Dir: t.TempDir(), Harness: h, Resume: resume}, r)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Stop("as_1") })
	return m, r, proto
}

func TestSessions(t *testing.T) {
	cases := map[string]string{
		core.ProtocolCodex:  "start:1 | message:echo hi (sandbox=workspace-write approval=rejected) | turn_completed:completed | ended:1",
		core.ProtocolACP:    "start:1 | message:echo hi (permission=selected yes) | turn_completed:end_turn | ended:1",
		core.ProtocolClaude: "start:1 | message:echo hi (resumed=false) | turn_completed:success | ended:1",
	}
	for protocol, want := range cases {
		t.Run(protocol, func(t *testing.T) {
			m, r, proto := start(t, fakeHarness(t, protocol), "")
			if proto == "" {
				t.Fatal("no protocol session ID")
			}
			turn, err := m.Prompt("as_1", "hi")
			if err != nil || turn != 1 {
				t.Fatalf("turn=%d err=%v", turn, err)
			}
			r.waitFor(t, "ended:1")
			if r.String() != want {
				t.Fatalf("log = %s", r)
			}

			// A second prompt while a turn hangs is rejected; Stop interrupts
			// it and reports nothing after it returned.
			if _, err := m.Prompt("as_1", "hang"); err != nil {
				t.Fatal(err)
			}
			if _, err := m.Prompt("as_1", "again"); !errors.Is(err, core.ErrSessionBusy) {
				t.Fatalf("concurrent prompt err = %v", err)
			}
			if err := m.Stop("as_1"); err != nil {
				t.Fatal(err)
			}
			afterStop := r.String()
			if strings.Contains(afterStop, "exited") || !strings.Contains(afterStop, "start:2") {
				t.Fatalf("log after stop = %s", afterStop)
			}
			if _, err := m.Prompt("as_1", "late"); !errors.Is(err, core.ErrSessionInactive) {
				t.Fatalf("prompt after stop err = %v", err)
			}
			if err := m.Stop("as_1"); !errors.Is(err, core.ErrSessionInactive) {
				t.Fatalf("second stop err = %v", err)
			}
			time.Sleep(50 * time.Millisecond) // a late report would show up here
			if r.String() != afterStop {
				t.Fatalf("reported after stop: %s", r)
			}
		})
	}
}

func TestTurnStartedErrorAbortsPrompt(t *testing.T) {
	m, r, _ := start(t, fakeHarness(t, core.ProtocolCodex), "")
	r.failNext = errors.New("store down")
	if _, err := m.Prompt("as_1", "hi"); err == nil || err.Error() != "store down" {
		t.Fatalf("err = %v", err)
	}
	// The aborted turn does not count and the session stays usable.
	if turn, err := m.Prompt("as_1", "hi"); err != nil || turn != 1 {
		t.Fatalf("turn=%d err=%v", turn, err)
	}
	r.waitFor(t, "ended:1")
}

func TestResume(t *testing.T) {
	const claudeID = "0b5e2c7e-4c1d-4f6a-9c3b-2a1d0e9f8b7a"
	cases := map[string]struct {
		protocol, resume string
		env              map[string]string
	}{
		"codex":              {core.ProtocolCodex, "previous-thread", nil},
		"acp session/load":   {core.ProtocolACP, "previous-session", nil},
		"acp session/resume": {core.ProtocolACP, "previous-session", map[string]string{"REACH_FAKE_ACP_RESUME": "1"}},
		"claude":             {core.ProtocolClaude, claudeID, nil},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			h := fakeHarness(t, c.protocol)
			for k, v := range c.env {
				h.Env[k] = v
			}
			_, r, proto := start(t, h, c.resume)
			if proto != c.resume {
				t.Fatalf("protocol session = %q", proto)
			}
			// History replayed by session/load is not reported as events.
			if r.String() != "" {
				t.Fatalf("replayed history reported: %s", r)
			}
		})
	}
	m := New(t.TempDir())
	if _, err := m.Start(context.Background(), "as_x", core.AgentStart{Dir: t.TempDir(),
		Harness: fakeHarness(t, core.ProtocolClaude), Resume: "not-a-uuid"}, newRecorder()); err == nil {
		t.Fatal("invalid Claude session id accepted")
	}
}

func TestClaudeFailedTurn(t *testing.T) {
	m, r, _ := start(t, fakeHarness(t, core.ProtocolClaude), "")
	if _, err := m.Prompt("as_1", "fail"); err != nil {
		t.Fatal(err)
	}
	r.waitFor(t, "ended:1")
	if r.String() != "start:1 | error:boom | turn_completed:error_during_execution | ended:1" {
		t.Fatalf("log = %s", r)
	}
}

func TestHarnessCrashIsReported(t *testing.T) {
	for _, protocol := range []string{core.ProtocolCodex, core.ProtocolClaude, core.ProtocolACP} {
		t.Run(protocol, func(t *testing.T) {
			m, r, _ := start(t, fakeHarness(t, protocol), "")
			if _, err := m.Prompt("as_1", "crash"); err != nil {
				t.Fatal(err)
			}
			// The exit is reported once, after the interrupted turn.
			r.waitFor(t, "exited:1")
			if log := r.String(); !strings.HasPrefix(log, "start:1 | error:") || strings.Contains(log, "| ended:") {
				t.Fatalf("log = %s", log)
			}
			if _, err := m.Prompt("as_1", "late"); !errors.Is(err, core.ErrSessionInactive) {
				t.Fatalf("prompt after crash err = %v", err)
			}
		})
	}
}

func TestStartFailure(t *testing.T) {
	h := fakeHarness(t, core.ProtocolACP)
	h.Env = map[string]string{"REACH_FAKE_AGENT": ""} // exits without speaking ACP
	m := New(t.TempDir())
	r := newRecorder()
	if _, err := m.Start(context.Background(), "as_1", core.AgentStart{Dir: t.TempDir(), Harness: h}, r); err == nil {
		t.Fatal("start succeeded")
	}
	if _, err := m.Prompt("as_1", "hi"); !errors.Is(err, core.ErrSessionInactive) {
		t.Fatalf("failed session is active: %v", err)
	}
	if r.String() != "" {
		t.Fatalf("failed start reported: %s", r)
	}
}
