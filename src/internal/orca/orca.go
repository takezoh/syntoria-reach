// Package orca drives the Orca server of this host through the Orca CLI's
// JSON output: the projects that are Reach workspaces, and the terminals of
// the agents Reach starts in them.
package orca

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
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

// listLimit is the most worktrees Reach reads at once; a longer list is an
// error rather than a silently partial answer.
const listLimit = 1000

// AddFolder registers the existing folder at path as an Orca folder project
// and returns its repo ID.
func (c *CLI) AddFolder(ctx context.Context, path string) (string, error) {
	var res struct {
		Repo core.Repo `json:"repo"`
	}
	if err := c.run(ctx, &res, "repo", "add", "--path", path, "--kind", "folder"); err != nil {
		return "", err
	}
	if res.Repo.ID == "" {
		return "", errors.New("orca: repo add returned no repo")
	}
	return res.Repo.ID, nil
}

// Repos lists the projects registered in Orca.
func (c *CLI) Repos(ctx context.Context) ([]core.Repo, error) {
	var res struct {
		Repos []core.Repo `json:"repos"`
	}
	if err := c.run(ctx, &res, "repo", "list"); err != nil {
		return nil, err
	}
	return res.Repos, nil
}

// Repo returns one project; core.ErrNotFound if Orca has none with that ID.
func (c *CLI) Repo(ctx context.Context, id string) (*core.Repo, error) {
	var res struct {
		Repo core.Repo `json:"repo"`
	}
	if err := c.run(ctx, &res, "repo", "show", "--repo", "id:"+id); err != nil {
		return nil, notFound(err)
	}
	return &res.Repo, nil
}

// Worktrees lists the workspace entries of one project, or of every project
// when repoID is empty; core.ErrNotFound if Orca has no such project.
func (c *CLI) Worktrees(ctx context.Context, repoID string) ([]core.Worktree, error) {
	var res struct {
		Worktrees []core.Worktree `json:"worktrees"`
		Truncated bool            `json:"truncated"`
	}
	args := []string{"worktree", "list"}
	if repoID != "" {
		args = append(args, "--repo", "id:"+repoID)
	}
	if err := c.run(ctx, &res, append(args, "--limit", fmt.Sprint(listLimit))...); err != nil {
		return nil, notFound(err)
	}
	if res.Truncated {
		return nil, fmt.Errorf("orca: more than %d worktrees", listLimit)
	}
	return res.Worktrees, nil
}

// SetWorktree changes a workspace entry's comment and workspace status; an
// empty value leaves that one unchanged.
func (c *CLI) SetWorktree(ctx context.Context, id, comment, status string) error {
	args := []string{"worktree", "set", "--worktree", "id:" + id}
	if comment != "" {
		args = append(args, "--comment="+comment)
	}
	if status != "" {
		args = append(args, "--workspace-status", status)
	}
	var res struct{}
	return c.run(ctx, &res, args...)
}

// DeleteProject removes a project from Orca, with its saved workspace
// details, and detaches its open terminals. Its folder stays on disk.
func (c *CLI) DeleteProject(ctx context.Context, repoID string) error {
	var res struct{}
	return c.run(ctx, &res, "project", "setup-delete", "--setup", repoID, "--force")
}

// promptSubmitWait is how long Orca watches a prompt it typed for the agent
// to start its turn.
const promptSubmitWait = 30

// CreateTerminal opens a terminal in the workspace entry worktreeID, types
// command into its shell, and returns the terminal's handle.
func (c *CLI) CreateTerminal(ctx context.Context, worktreeID, title, command string) (string, error) {
	var res struct {
		Terminal core.Terminal `json:"terminal"`
	}
	if err := c.run(ctx, &res, "terminal", "create", "--worktree", "id:"+worktreeID, "--title", title, "--command", command); err != nil {
		return "", err
	}
	if res.Terminal.Handle == "" {
		return "", errors.New("orca: terminal create returned no terminal")
	}
	return res.Terminal.Handle, nil
}

// Terminals lists the terminals Orca manages.
func (c *CLI) Terminals(ctx context.Context) ([]core.Terminal, error) {
	var res struct {
		Terminals []core.Terminal `json:"terminals"`
		Truncated bool            `json:"truncated"`
	}
	if err := c.run(ctx, &res, "terminal", "list"); err != nil {
		return nil, err
	}
	if res.Truncated {
		return nil, errors.New("orca: the terminal list is truncated")
	}
	return res.Terminals, nil
}

// SendPrompt types text into the agent running in terminal handle, submits
// it, and returns the stages Orca observed (input_accepted, turn_started).
func (c *CLI) SendPrompt(ctx context.Context, handle, text string) ([]string, error) {
	var res struct {
		Send struct {
			Accepted bool `json:"accepted"`
			Prompt   struct {
				Stages []string `json:"stages"`
			} `json:"prompt"`
		} `json:"send"`
	}
	if err := c.run(ctx, &res, "terminal", "send", "--terminal="+handle, "--text="+text, "--enter", "--wait-submit", fmt.Sprint(promptSubmitWait)); err != nil {
		return nil, err
	}
	if !res.Send.Accepted {
		return nil, errors.New("orca: the terminal did not accept the prompt")
	}
	return res.Send.Prompt.Stages, nil
}

// CloseTerminal closes a terminal and ends what runs in it.
func (c *CLI) CloseTerminal(ctx context.Context, handle string) error {
	var res struct{}
	return c.run(ctx, &res, "terminal", "close", "--terminal="+handle)
}

// ShowTerminal shows one terminal; core.ErrNotFound once Orca no longer has
// it.
func (c *CLI) ShowTerminal(ctx context.Context, handle string) (*core.TerminalStatus, error) {
	var res struct {
		Terminal struct {
			core.Terminal
			// Absent when Orca did not look, null when it saw no wait.
			AgentWait json.RawMessage `json:"agentWait"`
		} `json:"terminal"`
	}
	if err := c.run(ctx, &res, "terminal", "show", "--terminal="+handle); err != nil {
		return nil, terminalGone(err)
	}
	t := &core.TerminalStatus{Terminal: res.Terminal.Terminal}
	switch w := res.Terminal.AgentWait; {
	case len(w) == 0:
		t.WaitingOnHuman = core.WaitUnknown
	case string(w) != "null":
		var wait struct {
			Source string `json:"source"`
			Reason string `json:"reason"`
		}
		if err := json.Unmarshal(w, &wait); err != nil || wait.Source == "" {
			t.WaitingOnHuman = core.WaitUnknown
		} else if wait.Reason != "" {
			t.WaitingOnHuman = wait.Reason + " (" + wait.Source + ")"
		} else {
			t.WaitingOnHuman = wait.Source
		}
	}
	return t, nil
}

// WaitIdle waits up to timeout for Orca to see the agent in terminal handle
// idle at its prompt (tui-idle).
func (c *CLI) WaitIdle(ctx context.Context, handle string, timeout time.Duration) (bool, string, error) {
	var res struct {
		Wait struct {
			Satisfied     bool   `json:"satisfied"`
			BlockedReason string `json:"blockedReason"`
		} `json:"wait"`
	}
	err := c.run(ctx, &res, "terminal", "wait", "--terminal="+handle, "--for", "tui-idle", "--timeout-ms", fmt.Sprint(timeout.Milliseconds()))
	var oe *core.OrcaError
	if errors.As(err, &oe) && oe.Code == "timeout" {
		return false, "", nil
	}
	if err != nil {
		return false, "", terminalGone(err)
	}
	return res.Wait.Satisfied, res.Wait.BlockedReason, nil
}

// ReadTerminal reads terminal handle's output, escape sequences stripped,
// from cursor. Orca's cursor counts the terminal's completed lines, and a
// cursor below the oldest line it keeps reads from that line.
func (c *CLI) ReadTerminal(ctx context.Context, handle, cursor string, limit int) (*core.TerminalOutput, error) {
	if cursor == "" {
		cursor = "0"
	}
	args := []string{"terminal", "read", "--terminal=" + handle, "--cursor=" + cursor}
	if limit > 0 {
		args = append(args, "--limit", fmt.Sprint(limit))
	}
	var res struct {
		Terminal struct {
			Tail       []string `json:"tail"`
			NextCursor *string  `json:"nextCursor"` // null without line cursors
			Truncated  bool     `json:"truncated"`
			Limited    bool     `json:"limited"`
		} `json:"terminal"`
	}
	if err := c.run(ctx, &res, args...); err != nil {
		return nil, terminalGone(err)
	}
	out := &core.TerminalOutput{Lines: res.Terminal.Tail, Cursor: cursor, More: res.Terminal.Limited, Truncated: res.Terminal.Truncated}
	if res.Terminal.NextCursor != nil {
		out.Cursor = *res.Terminal.NextCursor
	}
	if out.Lines == nil {
		out.Lines = []string{}
	}
	return out, nil
}

// terminalGone turns Orca's answer for a handle it no longer has, or a
// terminal whose process ended, into core.ErrNotFound.
func terminalGone(err error) error {
	var oe *core.OrcaError
	if errors.As(err, &oe) {
		switch oe.Code {
		case "terminal_handle_stale", "terminal_gone", "terminal_not_found", "terminal_exited":
			return core.ErrNotFound
		}
	}
	return err
}

// notFound turns Orca's answer for an unknown project into
// core.ErrNotFound.
func notFound(err error) error {
	var oe *core.OrcaError
	if errors.As(err, &oe) && strings.HasSuffix(oe.Code, "_not_found") {
		return core.ErrNotFound
	}
	return err
}

// run runs the Orca CLI with args and decodes its result into out. A value
// a client chose, such as a terminal handle or a cursor, is passed as
// --flag=value, so that Orca never reads it as a flag of its own.
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
