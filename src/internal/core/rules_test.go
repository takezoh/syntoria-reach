package core

import (
	"errors"
	"fmt"
	"math"
	"testing"
	"time"
)

func TestValidNames(t *testing.T) {
	for id, want := range map[string]bool{
		"op-1": true, "a.b:c_d": true, "": false, "with space": false, "x/y": false,
		string(make([]byte, 129)): false,
	} {
		if ValidOperationID(id) != want {
			t.Errorf("ValidOperationID(%q) != %v", id, want)
		}
	}
	for name, want := range map[string]bool{
		"ws": true, "feature.x-1": true, "": false, "-x": false, ".x": false, "../x": false, "a/b": false,
	} {
		if ValidWorkspaceName(name) != want {
			t.Errorf("ValidWorkspaceName(%q) != %v", name, want)
		}
	}
}

func TestCheckPath(t *testing.T) {
	for _, ok := range []string{".", "a", "a/b.txt", "a/../b"} {
		if err := CheckPath(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "..", "../x", "/etc", "a/../../x", "a\x00b"} {
		if err := CheckPath(bad); !errors.Is(err, ErrInvalidPath) {
			t.Errorf("%q: %v", bad, err)
		}
	}
}

func TestArgsHash(t *testing.T) {
	type req struct {
		OperationID string   `json:"operation_id"`
		Path        string   `json:"path"`
		Args        []string `json:"args"`
	}
	r := req{OperationID: "a", Path: "p", Args: []string{"x"}}
	h1, err := ArgsHash(r)
	if err != nil {
		t.Fatal(err)
	}
	if h2, _ := ArgsHash(req{OperationID: "b", Path: "p", Args: []string{"x"}}); h2 != h1 {
		t.Error("the operation ID changes the hash")
	}
	if h3, _ := ArgsHash(req{OperationID: "a", Path: "p", Args: []string{"y"}}); h3 == h1 {
		t.Error("different arguments have the same hash")
	}
	// Field order does not matter: the same fields in another struct.
	type reordered struct {
		Args []string `json:"args"`
		Path string   `json:"path"`
	}
	if h4, _ := ArgsHash(reordered{Path: "p", Args: []string{"x"}}); h4 != h1 {
		t.Error("field order changes the hash")
	}
	if r.Args[0] != "x" || r.OperationID != "a" {
		t.Error("input changed")
	}
	if _, err := ArgsHash(func() {}); err == nil {
		t.Error("unencodable request accepted")
	}
}

func TestAdmit(t *testing.T) {
	stored := &Operation{Kind: "write_file", ArgsHash: "h"}
	cases := []struct {
		created    bool
		kind, hash string
		want       Admission
	}{
		{true, "write_file", "h", AdmitRun},
		{false, "write_file", "h", AdmitReplay},
		{false, "write_file", "other", AdmitConflict},
		{false, "run_command", "h", AdmitConflict},
	}
	for _, c := range cases {
		if got := Admit(stored, c.created, c.kind, c.hash); got != c.want {
			t.Errorf("Admit(created=%v, %s, %s) = %v, want %v", c.created, c.kind, c.hash, got, c.want)
		}
	}
}

func TestTimeout(t *testing.T) {
	const def, max = time.Minute, 10 * time.Minute
	cases := []struct {
		seconds int
		want    time.Duration
		ok      bool
	}{
		{0, def, true},
		{1, time.Second, true},
		{600, max, true},
		{601, 0, false},
		{-1, 0, false},
		{math.MaxInt, 0, false}, // overflows to a negative duration
		{math.MaxInt64 / int(time.Second) * 2, 0, false},
	}
	for _, c := range cases {
		got, err := Timeout(c.seconds, def, max)
		if (err == nil) != c.ok || got != c.want {
			t.Errorf("Timeout(%d) = %v, %v", c.seconds, got, err)
		}
		if err != nil && Classify(err).Code != CodeInvalidRequest {
			t.Errorf("Timeout(%d) error class = %v", c.seconds, err)
		}
	}
}

func TestJobOutcome(t *testing.T) {
	zero, one := 0, 1
	cases := []struct {
		exit       JobExit
		state, msg string
	}{
		{JobExit{ExitCode: &zero}, JobSucceeded, ""},
		{JobExit{ExitCode: &one}, JobFailed, ""},
		{JobExit{}, JobFailed, "terminated by a signal"},
		{JobExit{ExitCode: &zero, Err: "wait: broken"}, JobFailed, "wait: broken"},
		{JobExit{TimedOut: true}, JobFailed, "timed out"},
		// A cancel wins over the timeout and the signal it caused.
		{JobExit{Cancelled: true, TimedOut: true}, JobCancelled, ""},
		{JobExit{ExitCode: &zero, Truncated: true}, JobSucceeded, ""},
	}
	for _, c := range cases {
		if state, msg := JobOutcome(c.exit); state != c.state || msg != c.msg {
			t.Errorf("JobOutcome(%+v) = %s, %q; want %s, %q", c.exit, state, msg, c.state, c.msg)
		}
	}
}

func TestWorkspaceAccess(t *testing.T) {
	perms := []string{PermFileRead}
	cases := []struct {
		state string
		known bool
		perm  string
		want  bool
	}{
		{WorkspaceReady, true, PermFileRead, true},
		{WorkspaceReady, true, PermFileWrite, false},
		{WorkspaceReady, false, PermFileRead, false}, // project removed from the config
		{WorkspaceCreating, true, PermFileRead, false},
		{WorkspaceDeleting, true, PermFileRead, false},
		{WorkspaceDeleted, true, PermFileRead, false},
		{StateUnknown, true, PermFileRead, false},
	}
	for _, c := range cases {
		if got := WorkspaceAccess(&Workspace{State: c.state}, c.known, perms, c.perm); got != c.want {
			t.Errorf("WorkspaceAccess(%s, known=%v, %s) = %v", c.state, c.known, c.perm, got)
		}
	}
}

func TestDeletable(t *testing.T) {
	if err := Deletable(&Workspace{State: WorkspaceReady}, false); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		state string
		busy  bool
	}{{WorkspaceReady, true}, {WorkspaceDeleting, false}, {WorkspaceDeleted, false}, {WorkspaceFailed, false}} {
		if err := Deletable(&Workspace{State: c.state}, c.busy); Classify(err) == nil || Classify(err).Code != CodeInvalidState {
			t.Errorf("Deletable(%s, busy=%v) = %v", c.state, c.busy, err)
		}
	}
}

func TestResumable(t *testing.T) {
	prev := func(state, proto string) *AgentSession {
		return &AgentSession{ID: "as_1", WorkspaceID: "ws", AgentID: "codex", State: state, ProtocolSessionID: proto}
	}
	if id, err := Resumable(prev(SessionClosed, "thread"), "ws", "codex"); err != nil || id != "thread" {
		t.Fatalf("closed session: %q %v", id, err)
	}
	for _, state := range []string{SessionFailed, StateUnknown} {
		if id, err := Resumable(prev(state, "thread"), "ws", "codex"); err != nil || id != "thread" {
			t.Errorf("%s session: %q %v", state, id, err)
		}
	}
	cases := []struct {
		name          string
		prev          *AgentSession
		workspace, ag string
		code          string
	}{
		{"other workspace", prev(SessionClosed, "thread"), "ws2", "codex", CodeInvalidRequest},
		{"other agent", prev(SessionClosed, "thread"), "ws", "claude", CodeInvalidRequest},
		{"starting", prev(SessionStarting, ""), "ws", "codex", CodeInvalidState},
		{"idle", prev(SessionIdle, "thread"), "ws", "codex", CodeInvalidState},
		{"running", prev(SessionRunning, "thread"), "ws", "codex", CodeInvalidState},
		{"never opened", prev(SessionFailed, ""), "ws", "codex", CodeInvalidState},
	}
	for _, c := range cases {
		if _, err := Resumable(c.prev, c.workspace, c.ag); Classify(err) == nil || Classify(err).Code != c.code {
			t.Errorf("%s: %v", c.name, err)
		}
	}
}

func TestClassify(t *testing.T) {
	cases := map[error]string{
		Errorf(CodeOperationConflict, "x"):                  CodeOperationConflict,
		fmt.Errorf("wrapped: %w", Errorf(CodeNotFound, "")): CodeNotFound,
		&OrcaError{Code: "worktree_dirty", Message: "m"}:    CodeOrcaFailed,
		fmt.Errorf("%w: %q", ErrInvalidPath, "../x"):        CodeInvalidRequest,
		ErrNotRegular:      CodeInvalidRequest,
		ErrOutside:         CodePermissionDenied,
		ErrNotFound:        CodeNotFound,
		ErrVersionConflict: CodeVersionConflict,
		ErrSessionInactive: CodeInvalidState,
		ErrSessionBusy:     CodeInvalidState,
	}
	for err, want := range cases {
		if got := Classify(err); got == nil || got.Code != want {
			t.Errorf("Classify(%v) = %v, want %s", err, got, want)
		}
	}
	for _, err := range []error{errors.New("disk full"), ErrJobNotRunning} {
		if got := Classify(err); got != nil {
			t.Errorf("Classify(%v) = %v, want internal (nil)", err, got)
		}
	}
	if got := Classify(&OrcaError{Code: "c", Message: "m"}); got.Message != "c: m" {
		t.Errorf("orca message = %q", got.Message)
	}
}

func TestAuditOutcome(t *testing.T) {
	cases := []struct {
		err             error
		replayed, fails bool
		failCode        string
		outcome, code   string
	}{
		{nil, false, false, "", "ok", ""},
		{nil, true, false, "", "replayed", ""},
		{nil, false, true, CodeVersionConflict, "failed", CodeVersionConflict},
		{Errorf(CodePermissionDenied, ""), false, false, "", "denied", CodePermissionDenied},
		{Errorf(CodeUnauthorized, ""), false, false, "", "denied", CodeUnauthorized},
		{Errorf(CodeOperationConflict, ""), false, false, "", "error", CodeOperationConflict},
		{errors.New("unclassified"), false, false, "", "error", ""},
	}
	for _, c := range cases {
		if outcome, code := AuditOutcome(c.err, c.replayed, c.fails, c.failCode); outcome != c.outcome || code != c.code {
			t.Errorf("AuditOutcome(%v, %v, %v) = %s, %s", c.err, c.replayed, c.fails, outcome, code)
		}
	}
}
