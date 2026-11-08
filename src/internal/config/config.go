// Package config loads the Reach daemon configuration file.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"

	"github.com/takezoh/syntoria-reach/src/internal/core"
)

// Config is the on-disk daemon configuration. It contains client tokens, so the
// file must not be readable by group or others.
type Config struct {
	Socket     string   `json:"socket"`
	Database   string   `json:"database"`
	Workspaces string   `json:"workspaces"` // folder of the workspace folders Reach creates
	Orca       []string `json:"orca"`       // Orca CLI command; Command[0] is absolute
	Agents     []Agent  `json:"agents"`
	Commands   []string `json:"commands"` // absolute paths of the executables run_command and start_job may run
	Clients    []Client `json:"clients"`
	Remote     *Remote  `json:"remote"`
}

// Remote connects the daemon to the Reach gateway (Cloudflare) so that remote
// MCP clients reach it. Every request that arrives through the gateway acts
// as the client named Client, with that client's permissions.
type Remote struct {
	URL    string `json:"url"`    // the gateway's connect endpoint: wss://, or ws:// to a loopback host
	Token  string `json:"token"`  // the node token the gateway expects
	Client string `json:"client"` // ID of a client in Clients
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

// Client is a caller identified by a bearer token. Allow lists its
// permissions, which apply to every workspace.
type Client struct {
	ID    string   `json:"id"`
	Token string   `json:"token"`
	Allow []string `json:"allow"`
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
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields() // a key from an older format must not be silently ignored
	if err := dec.Decode(&c); err != nil {
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
	if c.Workspaces == "" {
		c.Workspaces = filepath.Join(filepath.Dir(c.Database), "workspaces")
	}
	return nil
}

// Validate checks the configuration and fills agent defaults. It has no
// side effects beyond c.
func (c *Config) Validate() error {
	if !filepath.IsAbs(c.Socket) || !filepath.IsAbs(c.Database) || !filepath.IsAbs(c.Workspaces) {
		return errors.New("socket, database and workspaces must be absolute paths")
	}
	if len(c.Orca) == 0 || !filepath.IsAbs(c.Orca[0]) {
		return errors.New("orca must be set to the Orca CLI command, starting with an absolute path")
	}
	for _, cmd := range c.Commands {
		if !filepath.IsAbs(cmd) {
			return fmt.Errorf("command %q must be an absolute path", cmd)
		}
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
		for _, p := range cl.Allow {
			switch p {
			case core.PermWorkspaceCreate, core.PermFileRead, core.PermFileWrite, core.PermCommandRun, core.PermAgentRun:
			default:
				return fmt.Errorf("client %s: unknown permission %q", cl.ID, p)
			}
		}
	}
	if r := c.Remote; r != nil {
		if err := checkRemoteURL(r.URL); err != nil {
			return fmt.Errorf("remote: %w", err)
		}
		if len(r.Token) < 32 {
			return errors.New("remote: token must be at least 32 characters")
		}
		if !clients[r.Client] {
			return fmt.Errorf("remote: unknown client %q", r.Client)
		}
	}
	return nil
}

// checkRemoteURL accepts wss:// URLs, and ws:// URLs to a loopback host (a
// gateway running locally with wrangler dev). The node token must never
// travel in the clear.
func checkRemoteURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return fmt.Errorf("url %q is not an absolute URL", raw)
	}
	switch u.Scheme {
	case "wss":
		return nil
	case "ws":
		host := u.Hostname()
		if ip := net.ParseIP(host); host == "localhost" || (ip != nil && ip.IsLoopback()) {
			return nil
		}
		return fmt.Errorf("url %q: ws:// is allowed only to a loopback host; use wss://", raw)
	}
	return fmt.Errorf("url %q must use wss://", raw)
}
