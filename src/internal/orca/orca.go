// Package orca drives the Orca server of this host through the Orca CLI's
// JSON output: the projects that are Reach workspaces, and the terminals and
// Dispatches of the agents Reach starts in them.
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
	if err := c.run(ctx, &res, "terminal", "send", "--terminal", handle, "--text="+text, "--enter", "--wait-submit", fmt.Sprint(promptSubmitWait)); err != nil {
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
	return c.run(ctx, &res, "terminal", "close", "--terminal", handle)
}

// AdoptTerminal makes terminal handle the Dispatch of a new Run and Task, so
// that Orca reads the transcript of the agent in it, and returns the
// Dispatch ID. The prompt is not injected into the terminal.
func (c *CLI) AdoptTerminal(ctx context.Context, handle, title string) (string, error) {
	var run struct {
		RunID string `json:"runId"`
	}
	if err := c.run(ctx, &run, "orchestration", "run-create", "--objective", title); err != nil {
		return "", err
	}
	var task struct {
		TaskID string `json:"taskId"`
	}
	if err := c.run(ctx, &task, "orchestration", "task-create", "--spec", title, "--task-title", title, "--run", run.RunID); err != nil {
		return "", err
	}
	var dispatch struct {
		Dispatch struct {
			ID string `json:"id"`
		} `json:"dispatch"`
	}
	if err := c.run(ctx, &dispatch, "orchestration", "dispatch", "--task", task.TaskID, "--to", handle, "--run", run.RunID); err != nil {
		return "", err
	}
	if run.RunID == "" || task.TaskID == "" || dispatch.Dispatch.ID == "" {
		return "", errors.New("orca: adopting the terminal returned no Dispatch")
	}
	return dispatch.Dispatch.ID, nil
}

// Dispatch shows a Dispatch; core.ErrNotFound if Orca has none.
func (c *CLI) Dispatch(ctx context.Context, id string) (*core.Dispatch, error) {
	var res core.Dispatch
	if err := c.run(ctx, &res, "orchestration", "worker-show", "--dispatch", id); err != nil {
		return nil, notFound(err)
	}
	if res.ID == "" {
		return nil, core.ErrNotFound
	}
	return &res, nil
}

// Transcript reads the conversation of the agent of Dispatch id from cursor
// (from the start when empty), at most limit messages (0 for Orca's
// default). core.ErrTranscriptUnavailable if Orca cannot read it yet.
func (c *CLI) Transcript(ctx context.Context, id, cursor string, limit int) (*core.Transcript, error) {
	args := []string{"orchestration", "worker-read", "--dispatch", id, "--source", "transcript"}
	if cursor != "" {
		args = append(args, "--cursor", cursor)
	}
	if limit > 0 {
		args = append(args, "--limit", fmt.Sprint(limit))
	}
	var res struct {
		Transcript struct {
			Messages   []json.RawMessage `json:"messages"`
			NextCursor string            `json:"nextCursor"`
		} `json:"transcript"`
		Status struct {
			Terminal string `json:"terminal"`
			Liveness string `json:"liveness"`
		} `json:"status"`
		Projection struct {
			Workspace struct {
				ID string `json:"id"`
			} `json:"workspace"`
			Stage struct {
				Activity string `json:"activity"`
			} `json:"stage"`
		} `json:"projection"`
	}
	if err := c.run(ctx, &res, args...); err != nil {
		var oe *core.OrcaError
		if errors.As(err, &oe) && oe.Code == "transcript_required" {
			return nil, core.ErrTranscriptUnavailable
		}
		return nil, notFound(err)
	}
	t := &core.Transcript{Messages: res.Transcript.Messages, Cursor: res.Transcript.NextCursor, Workspace: res.Projection.Workspace.ID,
		Activity: res.Projection.Stage.Activity, Terminal: res.Status.Terminal, Liveness: res.Status.Liveness}
	if t.Messages == nil {
		t.Messages = []json.RawMessage{}
	}
	return t, nil
}

// StopDispatch fences a Dispatch.
func (c *CLI) StopDispatch(ctx context.Context, id string) error {
	var res struct{}
	return c.run(ctx, &res, "orchestration", "worker-stop", "--dispatch", id)
}

// notFound turns Orca's answer for an unknown project or Dispatch into
// core.ErrNotFound.
func notFound(err error) error {
	var oe *core.OrcaError
	if errors.As(err, &oe) && strings.HasSuffix(oe.Code, "_not_found") {
		return core.ErrNotFound
	}
	return err
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
