// Package config loads the Reach daemon configuration file.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/takezoh/syntoria-reach/src/internal/core"
)

// Config is the on-disk daemon configuration. It contains client tokens, so the
// file must not be readable by group or others.
type Config struct {
	Socket   string    `json:"socket"`
	Database string    `json:"database"`
	Orca     []string  `json:"orca"` // Orca CLI command; Command[0] is absolute
	Agents   []Agent   `json:"agents"`
	Projects []Project `json:"projects"`
	Clients  []Client  `json:"clients"`
}

// Agent is an agent harness Reach can start (see core.Harness). The
// harness sandboxes its own tools: Sandbox defaults to "workspace-write" for
// Codex, and ACP permission requests are denied unless AllowPermissions.
type Agent struct {
	ID               string            `json:"id"`
	Protocol         string            `json:"protocol"`
	Command          []string          `json:"command"`
	Env              map[string]string `json:"env"`
	Sandbox          string            `json:"sandbox"`
	AllowPermissions bool              `json:"allow_permissions"`
}

// Harness returns how to start the agent.
func (a Agent) Harness() core.Harness {
	return core.Harness{Protocol: a.Protocol, Command: a.Command, Env: a.Env, Sandbox: a.Sandbox, AllowPermissions: a.AllowPermissions}
}

// Project is a repository registered in the Orca server from which clients
// create workspaces (Orca worktrees). Repo is its absolute path. Commands
// lists the absolute paths of executables and Agents the agent IDs allowed in
// its workspaces.
type Project struct {
	ID       string   `json:"id"`
	Repo     string   `json:"repo"`
	Commands []string `json:"commands"`
	Agents   []string `json:"agents"`
}

// Client is a caller identified by a bearer token.
type Client struct {
	ID     string  `json:"id"`
	Token  string  `json:"token"`
	Grants []Grant `json:"grants"`
}

// Grant gives a client permissions on one project.
type Grant struct {
	Project string   `json:"project"`
	Allow   []string `json:"allow"`
}

// Load reads and validates the configuration at path. Defaults come from
// the process environment.
func Load(path string) (*Config, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("config %s must not be accessible by group or others (mode %v)", path, info.Mode().Perm())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	home, _ := os.UserHomeDir()
	if err := c.ApplyDefaults(os.Getenv, home); err != nil {
		return nil, err
	}
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	return &c, nil
}

// ApplyDefaults fills unset paths from the environment (getenv) and the
// user's home directory.
func (c *Config) ApplyDefaults(getenv func(string) string, home string) error {
	if c.Socket == "" {
		dir := getenv("XDG_RUNTIME_DIR")
		if dir == "" {
			return errors.New("socket is not set and XDG_RUNTIME_DIR is empty")
		}
		c.Socket = filepath.Join(dir, "reach", "reach.sock")
	}
	if c.Database == "" {
		dir := getenv("XDG_STATE_HOME")
		if dir == "" {
			if home == "" {
				return errors.New("database is not set and the home directory is unknown")
			}
			dir = filepath.Join(home, ".local", "state")
		}
		c.Database = filepath.Join(dir, "reach", "reach.db")
	}
	return nil
}

// Validate checks the configuration and fills agent defaults. It has no
// side effects beyond c.
func (c *Config) Validate() error {
	if !filepath.IsAbs(c.Socket) || !filepath.IsAbs(c.Database) {
		return errors.New("socket and database must be absolute paths")
	}
	if len(c.Projects) > 0 && (len(c.Orca) == 0 || !filepath.IsAbs(c.Orca[0])) {
		return errors.New("orca must be set to the Orca CLI command, starting with an absolute path")
	}
	agents := map[string]bool{}
	for i := range c.Agents {
		a := &c.Agents[i]
		if a.ID == "" || agents[a.ID] {
			return fmt.Errorf("agent id %q is empty or duplicated", a.ID)
		}
		agents[a.ID] = true
		if a.Protocol != core.ProtocolCodex && a.Protocol != core.ProtocolClaude && a.Protocol != core.ProtocolACP {
			return fmt.Errorf("agent %s: protocol must be %q, %q or %q", a.ID, core.ProtocolCodex, core.ProtocolClaude, core.ProtocolACP)
		}
		if len(a.Command) == 0 || !filepath.IsAbs(a.Command[0]) {
			return fmt.Errorf("agent %s: command must start with an absolute path", a.ID)
		}
		if a.Protocol == core.ProtocolCodex && a.Sandbox == "" {
			a.Sandbox = "workspace-write"
		}
		switch {
		case a.Protocol != core.ProtocolCodex && a.Sandbox != "":
			return fmt.Errorf("agent %s: sandbox applies to codex agents only", a.ID)
		case a.Protocol != core.ProtocolACP && a.AllowPermissions:
			return fmt.Errorf("agent %s: allow_permissions applies to acp agents only", a.ID)
		case a.Protocol == core.ProtocolCodex && a.Sandbox != "read-only" && a.Sandbox != "workspace-write" && a.Sandbox != "danger-full-access":
			return fmt.Errorf("agent %s: unknown codex sandbox %q", a.ID, a.Sandbox)
		}
	}
	projects := map[string]bool{}
	for _, p := range c.Projects {
		if p.ID == "" || projects[p.ID] {
			return fmt.Errorf("project id %q is empty or duplicated", p.ID)
		}
		projects[p.ID] = true
		if !filepath.IsAbs(p.Repo) {
			return fmt.Errorf("project %s: repo must be an absolute path", p.ID)
		}
		for _, cmd := range p.Commands {
			if !filepath.IsAbs(cmd) {
				return fmt.Errorf("project %s: command %q must be an absolute path", p.ID, cmd)
			}
		}
		for _, a := range p.Agents {
			if !agents[a] {
				return fmt.Errorf("project %s: unknown agent %q", p.ID, a)
			}
		}
	}
	clients := map[string]bool{}
	tokens := map[string]bool{}
	for _, cl := range c.Clients {
		if cl.ID == "" || clients[cl.ID] {
			return fmt.Errorf("client id %q is empty or duplicated", cl.ID)
		}
		clients[cl.ID] = true
		if len(cl.Token) < 32 {
			return fmt.Errorf("client %s: token must be at least 32 characters", cl.ID)
		}
		if tokens[cl.Token] {
			return fmt.Errorf("client %s: token is shared with another client", cl.ID)
		}
		tokens[cl.Token] = true
		for _, g := range cl.Grants {
			if !projects[g.Project] {
				return fmt.Errorf("client %s: unknown project %q", cl.ID, g.Project)
			}
			for _, p := range g.Allow {
				switch p {
				case core.PermWorkspaceCreate, core.PermFileRead, core.PermFileWrite, core.PermCommandRun, core.PermAgentRun:
				default:
					return fmt.Errorf("client %s: unknown permission %q", cl.ID, p)
				}
			}
		}
	}
	return nil
}
