package orca

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
	id, err := cli.AdoptTerminal(ctx, h, "reach claude")
	if err != nil || id == "" {
		t.Fatalf("adopt: %q %v", id, err)
	}
	if d, err := cli.Dispatch(ctx, id); err != nil || d.Terminal != h || d.Liveness != "live" {
		t.Fatalf("dispatch = %+v %v", d, err)
	}
	// Nothing is readable before the agent reported its session.
	if _, err := cli.Transcript(ctx, id, "", 0); !errors.Is(err, core.ErrTranscriptUnavailable) {
		t.Fatalf("transcript before a prompt: %v", err)
	}
	// A prompt starting with "--" stays the text.
	if stages, err := cli.SendPrompt(ctx, h, "--hello"); err != nil || strings.Join(stages, ",") != "input_accepted,turn_started" {
		t.Fatalf("send: %q %v", stages, err)
	}
	tr, err := cli.Transcript(ctx, id, "", 0)
	if err != nil || len(tr.Messages) != 2 || tr.Workspace != entry || tr.Activity != "done" || tr.Terminal != "running" || tr.Cursor == "" {
		t.Fatalf("transcript = %+v %v", tr, err)
	}
	if !strings.Contains(string(tr.Messages[1]), "echo: --hello") {
		t.Fatalf("reply = %s", tr.Messages[1])
	}
	if more, err := cli.Transcript(ctx, id, tr.Cursor, 10); err != nil || len(more.Messages) != 0 {
		t.Fatalf("read on: %+v %v", more, err)
	}
	if err := cli.StopDispatch(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := cli.CloseTerminal(ctx, h); err != nil {
		t.Fatal(err)
	}
	if terms, _ := cli.Terminals(ctx); len(terms) != 0 {
		t.Fatalf("closed terminal listed: %+v", terms)
	}
	if _, err := cli.Dispatch(ctx, "ctx_nope"); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("unknown dispatch: %v", err)
	}
	calls := orcatest.Calls(t, root)
	for _, want := range []string{
		"terminal create --worktree id:" + entry + " --title reach claude --command /bin/claude --model 'opus x' --json",
		"orchestration dispatch --task ",
		"terminal send --terminal " + h + " --text=--hello --enter --wait-submit 30 --json",
		"orchestration worker-read --dispatch " + id + " --source transcript --json",
		"orchestration worker-stop --dispatch " + id + " --json",
		"terminal close --terminal " + h + " --json",
	} {
		if !strings.Contains(calls, want) {
			t.Errorf("missing call %q in:\n%s", want, calls)
		}
	}
}
