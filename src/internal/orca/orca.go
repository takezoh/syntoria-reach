// Package orca creates and removes workspaces (Git worktrees) on the Orca
// server of this host through the Orca CLI's JSON output.
package orca

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/takezoh/syntoria-reach/src/internal/command"
	"github.com/takezoh/syntoria-reach/src/internal/core"
)

const timeout = 2 * time.Minute

// CLI runs the Orca CLI. The CLI gets the daemon's environment, which it
// needs to reach the Orca runtime; it is Reach's own adapter, not a command
// chosen by a client.
type CLI struct {
	Command []string
}

// CreateWorktree creates a new worktree of the registered repo at repoPath.
// Repo setup hooks and agent launches are skipped; Reach starts agents itself.
func (c *CLI) CreateWorktree(ctx context.Context, repoPath, name string) (*core.Worktree, error) {
	var res struct {
		Worktree core.Worktree `json:"worktree"`
	}
	if err := c.run(ctx, &res, "worktree", "create", "--repo", "path:"+repoPath, "--name", name, "--no-parent", "--setup", "skip"); err != nil {
		return nil, err
	}
	if res.Worktree.ID == "" || res.Worktree.Path == "" {
		return nil, errors.New("orca: worktree create returned no worktree")
	}
	return &res.Worktree, nil
}

// RemoveWorktree removes a worktree from Orca and Git and deletes its branch
// when Orca can prove it is merged. Uncommitted changes make it fail.
func (c *CLI) RemoveWorktree(ctx context.Context, id string) error {
	var res struct {
		Removed bool `json:"removed"`
	}
	if err := c.run(ctx, &res, "worktree", "rm", "--worktree", "id:"+id, "--delete-branch"); err != nil {
		return err
	}
	if !res.Removed {
		return errors.New("orca: worktree rm did not confirm removal")
	}
	return nil
}

func (c *CLI) run(ctx context.Context, out any, args ...string) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	argv := append(append(append([]string{}, c.Command[1:]...), args...), "--json")
	cmd := command.Command(ctx, c.Command[0], argv, "", os.Environ())
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &command.LimitedWriter{W: &stdout, N: 4 << 20}
	cmd.Stderr = &command.LimitedWriter{W: &stderr, N: 64 << 10}
	runErr := cmd.Run()
	var resp struct {
		OK     bool            `json:"ok"`
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &resp); err != nil {
		return fmt.Errorf("orca %s: %v (stderr: %s)", args[0]+" "+args[1], errors.Join(runErr, err), bytes.TrimSpace(stderr.Bytes()))
	}
	if !resp.OK {
		if resp.Error != nil {
			return &core.OrcaError{Code: resp.Error.Code, Message: resp.Error.Message}
		}
		return fmt.Errorf("orca %s failed", args[0]+" "+args[1])
	}
	return json.Unmarshal(resp.Result, out)
}
