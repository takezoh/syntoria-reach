package core

import (
	"errors"
	"fmt"
	"math"
	"strings"
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
}

func TestWorkspaceName(t *testing.T) {
	for name, want := range map[string]string{
		"ws": "reach-ws", "feature.x-1": "reach-feature.x-1", "reach-ws": "reach-ws", "-x": "reach--x",
	} {
		if got, ok := WorkspaceName(name); !ok || got != want {
			t.Errorf("WorkspaceName(%q) = %q, %v; want %q", name, got, ok, want)
		}
	}
	for _, name := range []string{"", "reach-", "../x", "a/b", "x y", strings.Repeat("a", 64)} {
		if got, ok := WorkspaceName(name); ok {
			t.Errorf("WorkspaceName(%q) = %q accepted", name, got)
		}
	}
}

func TestIsWorkspace(t *testing.T) {
	for name, want := range map[string]bool{"reach-ws": true, "reach-": true, "syntoria-reach": false, "Reach-ws": false, "": false} {
		if got := IsWorkspace(&Repo{DisplayName: name}); got != want {
			t.Errorf("IsWorkspace(%q) = %v", name, got)
		}
	}
	r := &Repo{ID: "r1", DisplayName: "reach-ws", Path: "/w/reach-ws", Kind: "folder"}
	if w := NewWorkspace(r, &Worktree{Comment: "fix the docs", WorkspaceStatus: "in-review"}); *w != (Workspace{ID: "r1", Name: "reach-ws", Note: "fix the docs", Status: "in-review", Path: "/w/reach-ws"}) {
		t.Fatalf("NewWorkspace = %+v", w)
	}
	if w := NewWorkspace(r, nil); w.Note != "" || w.Status != "" || w.ID != "r1" {
		t.Fatalf("NewWorkspace without a root = %+v", w)
	}
}

func TestExpired(t *testing.T) {
	now := time.Date(2026, 11, 8, 0, 0, 0, 0, time.UTC)
	old, recent := now.Add(-CleanupAfter), now.Add(-CleanupAfter+time.Minute)
	for _, c := range []struct {
		status string
		last   time.Time
		busy   bool
		want   bool
	}{
		{WorkspaceCompleted, old, false, true},
		{WorkspaceCompleted, recent, false, false},
		{WorkspaceCompleted, old, true, false},
		{WorkspaceInProgress, old, false, false},
		{"in-review", old, false, false},
	} {
		if got := Expired(c.status, c.last, now, c.busy); got != c.want {
			t.Errorf("Expired(%s, %v, busy=%v) = %v", c.status, now.Sub(c.last), c.busy, got)
		}
	}
}

func TestOwnFolder(t *testing.T) {
	if !OwnFolder("/w", "/w/reach-ws", "reach-ws") {
		t.Fatal("own folder refused")
	}
	for _, c := range [][3]string{
		{"/w", "/w/reach-ws", "reach-other"},      // another name
		{"/w", "/elsewhere/reach-ws", "reach-ws"}, // outside the base
		{"/w", "/w/sub/reach-ws", "reach-ws"},
		{"/w", "/w", "reach-ws"},
		{"/w", "/w/ws", "ws"}, // no prefix
		{"w", "w/reach-ws", "reach-ws"},
		{"/w", "/w/reach-ws/..", "reach-ws"},
	} {
		if OwnFolder(c[0], c[1], c[2]) {
			t.Errorf("OwnFolder(%q, %q, %q) accepted", c[0], c[1], c[2])
		}
	}
}

func TestCheckNote(t *testing.T) {
	for _, ok := range []string{"x", "Fix the README.\nThen run the tests.", strings.Repeat("あ", MaxNoteLength)} {
		if err := CheckNote(ok); err != nil {
			t.Errorf("CheckNote(%.20q) = %v", ok, err)
		}
	}
	for _, bad := range []string{"", " \n\t", strings.Repeat("a", MaxNoteLength+1), "a\x00b", "\xff"} {
		if err := CheckNote(bad); Classify(err) == nil || Classify(err).Code != CodeInvalidRequest {
			t.Errorf("CheckNote(%.20q) = %v", bad, err)
		}
	}
	for status, want := range map[string]bool{"todo": true, "in-review": true, "my_status": true, "": false, "In-Review": false, "-x": false, "a b": false} {
		if ValidWorkspaceStatus(status) != want {
			t.Errorf("ValidWorkspaceStatus(%q) != %v", status, want)
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

func TestShellCommand(t *testing.T) {
	for want, argv := range map[string][]string{
		"/bin/claude --model claude-opus-5-5 --effort xhigh": {"/bin/claude", "--model", "claude-opus-5-5", "--effort", "xhigh"},
		`/bin/x 'a b' '$HOME' 'it'\''s' ''`:                  {"/bin/x", "a b", "$HOME", "it's", ""},
		"/bin/x --opt=v,w a@b%c":                             {"/bin/x", "--opt=v,w", "a@b%c"},
		`/bin/x ';rm -rf ~' '$(id)' '*'`:                     {"/bin/x", ";rm -rf ~", "$(id)", "*"},
	} {
		if got := ShellCommand(argv); got != want {
			t.Errorf("ShellCommand(%q) = %s, want %s", argv, got, want)
		}
	}
	if WorkspaceOfEntry("repo-1::/w/reach-ws") != "repo-1" || WorkspaceOfEntry("repo-1") != "repo-1" {
		t.Fatal("WorkspaceOfEntry")
	}
}

func TestCheckPrompt(t *testing.T) {
	for _, ok := range []string{"x", "Fix the README.\nThen run\tthe tests.", "  hello", "a/b!", "日本語", strings.Repeat("a", MaxPromptBytes)} {
		if err := CheckPrompt(ok); err != nil {
			t.Errorf("CheckPrompt(%.20q) = %v", ok, err)
		}
	}
	for _, bad := range []string{"", " \n\t", "!ls", "  !ls", "/cost", "\n/exit", "a\x1b[201~b", "a\rb", "a\x00b", "a\u0085b", "\xff", strings.Repeat("a", MaxPromptBytes+1)} {
		if err := CheckPrompt(bad); err == nil {
			t.Errorf("CheckPrompt(%.20q) accepted", bad)
		}
	}
}

func TestCheckCursor(t *testing.T) {
	for _, ok := range []string{"", "0", "547", strings.Repeat("9", 18)} {
		if err := CheckCursor(ok); err != nil {
			t.Errorf("CheckCursor(%q) = %v", ok, err)
		}
	}
	// Never a flag or another value for Orca's command line.
	for _, bad := range []string{"-1", "--terminal=term_other", "--environment", "1 2", "1e3", "+1", "٣", strings.Repeat("9", 19)} {
		if err := CheckCursor(bad); err == nil {
			t.Errorf("CheckCursor(%q) accepted", bad)
		}
	}
}
