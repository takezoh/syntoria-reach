package orca

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/takezoh/syntoria-reach/src/internal/orca/orcatest"
)

// TestFakeOrca is not a test: it turns the test binary into a fake Orca CLI.
func TestFakeOrca(t *testing.T) {
	orcatest.MaybeRun()
	t.Skip("helper process")
}

func TestWorktreeLifecycle(t *testing.T) {
	root := t.TempDir()
	cli := &CLI{Command: orcatest.Command(t, "TestFakeOrca", root)}
	ctx := context.Background()

	wt, err := cli.CreateWorktree(ctx, "/repo", "feature")
	if err != nil {
		t.Fatal(err)
	}
	if wt.Path != filepath.Join(root, "feature") || wt.Branch != "refs/heads/feature" || wt.ID == "" {
		t.Fatalf("worktree = %+v", wt)
	}
	if err := cli.RemoveWorktree(ctx, wt.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(wt.Path); !os.IsNotExist(err) {
		t.Fatal("worktree directory still exists")
	}
	calls := orcatest.Calls(t, root)
	for _, want := range []string{
		"worktree create --repo path:/repo --name feature --no-parent --setup skip --json",
		"worktree rm --worktree id:" + wt.ID + " --delete-branch --json",
	} {
		if !strings.Contains(calls, want) {
			t.Errorf("missing call %q in:\n%s", want, calls)
		}
	}
}

func TestOrcaErrors(t *testing.T) {
	root := t.TempDir()
	cli := &CLI{Command: orcatest.Command(t, "TestFakeOrca", root)}
	var oe *Error
	if _, err := cli.CreateWorktree(context.Background(), "/repo", "fail"); !errors.As(err, &oe) || oe.Code != "git_failed" {
		t.Fatalf("create error = %v", err)
	}
	wt, _ := cli.CreateWorktree(context.Background(), "/repo", "dirty-one")
	os.WriteFile(filepath.Join(wt.Path, "dirty"), nil, 0o644)
	if err := cli.RemoveWorktree(context.Background(), wt.ID); !errors.As(err, &oe) || oe.Code != "worktree_dirty" {
		t.Fatalf("remove error = %v", err)
	}
	broken := &CLI{Command: []string{"/bin/echo", "not json"}}
	if _, err := broken.CreateWorktree(context.Background(), "/repo", "x"); err == nil {
		t.Fatal("non-JSON output accepted")
	}
}
