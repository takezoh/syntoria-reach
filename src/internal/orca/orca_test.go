package orca

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/takezoh/syntoria-reach/src/internal/core"
	"github.com/takezoh/syntoria-reach/src/internal/orca/orcatest"
)

// TestFakeOrca is not a test: it turns the test binary into a fake Orca CLI.
func TestFakeOrca(t *testing.T) {
	orcatest.MaybeRun()
	t.Skip("helper process")
}

func TestProjectLifecycle(t *testing.T) {
	root := t.TempDir()
	cli := &CLI{Command: orcatest.Command(t, "TestFakeOrca", root)}
	ctx := context.Background()
	folder := filepath.Join(root, "reach-docs")
	os.Mkdir(folder, 0o755)

	id, err := cli.AddFolder(ctx, folder)
	if err != nil || id == "" {
		t.Fatalf("add: %q %v", id, err)
	}
	r, err := cli.Repo(ctx, id)
	if err != nil || *r != (core.Repo{ID: id, DisplayName: "reach-docs", Path: folder, Kind: "folder"}) {
		t.Fatalf("repo = %+v %v", r, err)
	}
	if repos, err := cli.Repos(ctx); err != nil || len(repos) != 1 || repos[0].ID != id {
		t.Fatalf("repos = %+v %v", repos, err)
	}
	wts, err := cli.Worktrees(ctx, id)
	if err != nil || len(wts) != 1 || !wts[0].IsMainWorktree || wts[0].RepoID != id || wts[0].LastActivityAt == 0 {
		t.Fatalf("worktrees = %+v %v", wts, err)
	}
	// A note starting with "--" stays a value; comment and status change
	// independently.
	rootID := wts[0].ID
	if err := cli.SetWorktree(ctx, rootID, "--write the docs", ""); err != nil {
		t.Fatal(err)
	}
	if err := cli.SetWorktree(ctx, rootID, "", "completed"); err != nil {
		t.Fatal(err)
	}
	if all, err := cli.Worktrees(ctx, ""); err != nil || len(all) != 1 || all[0].Comment != "--write the docs" || all[0].WorkspaceStatus != "completed" {
		t.Fatalf("all worktrees = %+v %v", all, err)
	}

	if err := cli.DeleteProject(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := cli.Repo(ctx, id); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("deleted project: %v", err)
	}
	if _, err := cli.Worktrees(ctx, id); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("worktrees of a deleted project: %v", err)
	}
	if _, err := os.Stat(folder); err != nil {
		t.Fatal("Orca removed the folder")
	}
	calls := orcatest.Calls(t, root)
	for _, want := range []string{
		"repo add --path " + folder + " --kind folder --json",
		"repo show --repo id:" + id + " --json",
		"worktree list --repo id:" + id + " --limit 1000 --json",
		"worktree list --limit 1000 --json",
		"worktree set --worktree id:" + rootID + " --comment=--write the docs --json",
		"worktree set --worktree id:" + rootID + " --workspace-status completed --json",
		"project setup-delete --setup " + id + " --force --json",
	} {
		if !strings.Contains(calls, want) {
			t.Errorf("missing call %q in:\n%s", want, calls)
		}
	}
}

func TestOrcaErrors(t *testing.T) {
	root := t.TempDir()
	cli := &CLI{Command: orcatest.Command(t, "TestFakeOrca", root)}
	ctx := context.Background()
	var oe *core.OrcaError
	os.Mkdir(filepath.Join(root, "reach-fail"), 0o755)
	if _, err := cli.AddFolder(ctx, filepath.Join(root, "reach-fail")); !errors.As(err, &oe) || oe.Code != "add_failed" {
		t.Fatalf("add error = %v", err)
	}
	if _, err := cli.Repo(ctx, "nope"); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("unknown repo = %v", err)
	}
	if err := cli.SetWorktree(ctx, "nope::/x", "x", ""); !errors.As(err, &oe) || oe.Code != "selector_not_found" {
		t.Fatalf("set on a missing entry = %v", err)
	}
	broken := &CLI{Command: []string{"/bin/echo", "not json"}}
	if _, err := broken.Repos(ctx); err == nil {
		t.Fatal("non-JSON output accepted")
	}
	// A partial list is an error, never a silently short answer.
	truncated := &CLI{Command: []string{"/bin/echo", `{"ok":true,"result":{"worktrees":[],"truncated":true}}`}}
	if _, err := truncated.Worktrees(ctx, ""); err == nil {
		t.Fatal("truncated list accepted")
	}
}

func TestAgentTerminal(t *testing.T) {
	root := t.TempDir()
	cli := &CLI{Command: orcatest.Command(t, "TestFakeOrca", root)}
	ctx := context.Background()
	entry := "repo-1::/w/reach-ws"

	h, err := cli.CreateTerminal(ctx, entry, "reach claude", "/bin/claude --model 'opus x'")
	if err != nil || h == "" {
		t.Fatalf("create: %q %v", h, err)
	}
	if terms, err := cli.Terminals(ctx); err != nil || len(terms) != 1 || terms[0] != (core.Terminal{Handle: h, WorktreeID: entry, Connected: true, AgentIdentity: "claude"}) {
		t.Fatalf("terminals = %+v %v", terms, err)
	}
	want := core.TerminalStatus{Terminal: core.Terminal{Handle: h, WorktreeID: entry, Connected: true, AgentIdentity: "claude"}}
	if st, err := cli.ShowTerminal(ctx, h); err != nil || *st != want {
		t.Fatalf("show = %+v %v", st, err)
	}
	if idle, blocked, err := cli.WaitIdle(ctx, h, 3*time.Second); err != nil || !idle || blocked != "" {
		t.Fatalf("wait = %v %q %v", idle, blocked, err)
	}
	if out, err := cli.ReadTerminal(ctx, h, "", 0); err != nil || len(out.Lines) != 0 || out.Cursor != "0" || out.More {
		t.Fatalf("output before a prompt = %+v %v", out, err)
	}
	// A prompt starting with "--" stays the text.
	if stages, err := cli.SendPrompt(ctx, h, "--hello"); err != nil || strings.Join(stages, ",") != "input_accepted,turn_started" {
		t.Fatalf("send: %q %v", stages, err)
	}
	out, err := cli.ReadTerminal(ctx, h, "", 1)
	if err != nil || strings.Join(out.Lines, "|") != "you: --hello" || out.Cursor != "1" || !out.More {
		t.Fatalf("first line = %+v %v", out, err)
	}
	if out, err = cli.ReadTerminal(ctx, h, out.Cursor, 0); err != nil || strings.Join(out.Lines, "|") != "claude: echo: --hello" || out.Cursor != "2" || out.More {
		t.Fatalf("read on = %+v %v", out, err)
	}
	if err := cli.CloseTerminal(ctx, h); err != nil {
		t.Fatal(err)
	}
	if terms, _ := cli.Terminals(ctx); len(terms) != 0 {
		t.Fatalf("closed terminal listed: %+v", terms)
	}
	// Orca no longer has the handle of a closed terminal.
	if _, err := cli.ShowTerminal(ctx, h); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("show a closed terminal: %v", err)
	}
	if _, _, err := cli.WaitIdle(ctx, h, time.Second); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("wait on a closed terminal: %v", err)
	}
	if _, err := cli.ReadTerminal(ctx, h, "0", 0); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("read a closed terminal: %v", err)
	}
	// A handle that looks like a flag stays the handle.
	if _, err := cli.ShowTerminal(ctx, "--environment=elsewhere"); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("show a handle that looks like a flag: %v", err)
	}
	calls := orcatest.Calls(t, root)
	for _, want := range []string{
		"terminal create --worktree id:" + entry + " --title reach claude --command /bin/claude --model 'opus x' --json",
		"terminal show --terminal=" + h + " --json",
		"terminal wait --terminal=" + h + " --for tui-idle --timeout-ms 3000 --json",
		"terminal read --terminal=" + h + " --cursor=0 --json",
		"terminal read --terminal=" + h + " --cursor=0 --limit 1 --json",
		"terminal send --terminal=" + h + " --text=--hello --enter --wait-submit 30 --json",
		"terminal close --terminal=" + h + " --json",
	} {
		if !strings.Contains(calls, want) {
			t.Errorf("missing call %q in:\n%s", want, calls)
		}
	}
}

// answering is an Orca CLI that answers every call with $REPLY_JSON.
var answering = &CLI{Command: []string{"/bin/sh", "-c", `printf '%s\n' "$REPLY_JSON"`, "sh"}}

// What Orca saw the agent wait for, and the answers of tui-idle, as the
// real Orca reports them.
func TestTerminalWaits(t *testing.T) {
	ctx := context.Background()
	show := func(wait string) string {
		return `{"ok":true,"result":{"terminal":{"handle":"term_1","worktreeId":"r::/w","connected":true,"agentIdentity":"claude"` + wait + `}}}`
	}
	for reply, want := range map[string]string{
		show(`,"agentWait":null`): "",
		show(``):                  core.WaitUnknown, // Orca did not look
		show(`,"agentWait":{"source":"hook","since":1794132705694}`):         "hook",
		show(`,"agentWait":{"source":"prompt-text","reason":"Allow Bash?"}`): "Allow Bash? (prompt-text)",
	} {
		t.Setenv("REPLY_JSON", reply)
		if st, err := answering.ShowTerminal(ctx, "term_1"); err != nil || st.WaitingOnHuman != want || st.AgentIdentity != "claude" {
			t.Errorf("%s: %+v %v", reply, st, err)
		}
	}
	// A shell is not an agent: Orca reports no agent identity.
	t.Setenv("REPLY_JSON", `{"ok":true,"result":{"terminal":{"handle":"term_1","worktreeId":"r::/w","connected":true,"agentIdentity":null,"agentWait":null}}}`)
	if st, err := answering.ShowTerminal(ctx, "term_1"); err != nil || st.AgentIdentity != "" || st.WaitingOnHuman != "" {
		t.Errorf("shell: %+v %v", st, err)
	}
	// An agent in a turn, or at a permission prompt, is not idle by the
	// deadline; Orca answers timeout.
	t.Setenv("REPLY_JSON", `{"ok":false,"error":{"code":"timeout","message":"timeout"}}`)
	if idle, blocked, err := answering.WaitIdle(ctx, "term_1", time.Second); err != nil || idle || blocked != "" {
		t.Errorf("timeout: %v %q %v", idle, blocked, err)
	}
	t.Setenv("REPLY_JSON", `{"ok":true,"result":{"wait":{"handle":"term_1","condition":"tui-idle","satisfied":false,"status":"running","exitCode":null,"blockedReason":"permission"}}}`)
	if idle, blocked, err := answering.WaitIdle(ctx, "term_1", time.Second); err != nil || idle || blocked != "permission" {
		t.Errorf("blocked: %v %q %v", idle, blocked, err)
	}
	t.Setenv("REPLY_JSON", `{"ok":false,"error":{"code":"terminal_exited","message":"terminal_exited"}}`)
	if _, _, err := answering.WaitIdle(ctx, "term_1", time.Second); !errors.Is(err, core.ErrNotFound) {
		t.Errorf("exited: %v", err)
	}
	// Without line cursors Orca returns no next cursor: read on from the same.
	t.Setenv("REPLY_JSON", `{"ok":true,"result":{"terminal":{"handle":"term_1","status":"running","tail":[],"truncated":true,"nextCursor":null}}}`)
	if out, err := answering.ReadTerminal(ctx, "term_1", "7", 0); err != nil || out.Cursor != "7" || !out.Truncated || out.Lines == nil {
		t.Errorf("no cursors: %+v %v", out, err)
	}
}
