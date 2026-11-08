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
