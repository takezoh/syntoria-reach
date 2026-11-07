// Package config loads the Reach daemon configuration file.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Permissions a client can be granted on a workspace.
const (
	PermFileRead   = "file.read"
	PermFileWrite  = "file.write"
	PermCommandRun = "command.run"
)

// Config is the on-disk daemon configuration. It contains client tokens, so the
// file must not be readable by group or others.
type Config struct {
	Socket     string      `json:"socket"`
	Database   string      `json:"database"`
	Workspaces []Workspace `json:"workspaces"`
	Clients    []Client    `json:"clients"`
}

// Workspace is a directory the daemon exposes. Commands lists the absolute
// paths of executables that may be run in it.
type Workspace struct {
	ID       string   `json:"id"`
	Root     string   `json:"root"`
	Commands []string `json:"commands"`
}

// Client is a caller identified by a bearer token.
type Client struct {
	ID     string  `json:"id"`
	Token  string  `json:"token"`
	Grants []Grant `json:"grants"`
}

// Grant gives a client permissions on one workspace.
type Grant struct {
	Workspace string   `json:"workspace"`
	Allow     []string `json:"allow"`
}

// Load reads and validates the configuration at path.
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
	if err := c.applyDefaults(); err != nil {
		return nil, err
	}
	if err := c.validate(); err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	return &c, nil
}

func (c *Config) applyDefaults() error {
	if c.Socket == "" {
		dir := os.Getenv("XDG_RUNTIME_DIR")
		if dir == "" {
			return errors.New("socket is not set and XDG_RUNTIME_DIR is empty")
		}
		c.Socket = filepath.Join(dir, "reach", "reach.sock")
	}
	if c.Database == "" {
		dir := os.Getenv("XDG_STATE_HOME")
		if dir == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				return err
			}
			dir = filepath.Join(home, ".local", "state")
		}
		c.Database = filepath.Join(dir, "reach", "reach.db")
	}
	return nil
}

func (c *Config) validate() error {
	if !filepath.IsAbs(c.Socket) || !filepath.IsAbs(c.Database) {
		return errors.New("socket and database must be absolute paths")
	}
	workspaces := map[string]bool{}
	for _, w := range c.Workspaces {
		if w.ID == "" || workspaces[w.ID] {
			return fmt.Errorf("workspace id %q is empty or duplicated", w.ID)
		}
		workspaces[w.ID] = true
		if !filepath.IsAbs(w.Root) {
			return fmt.Errorf("workspace %s: root must be an absolute path", w.ID)
		}
		for _, cmd := range w.Commands {
			if !filepath.IsAbs(cmd) {
				return fmt.Errorf("workspace %s: command %q must be an absolute path", w.ID, cmd)
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
			if !workspaces[g.Workspace] {
				return fmt.Errorf("client %s: unknown workspace %q", cl.ID, g.Workspace)
			}
			for _, p := range g.Allow {
				switch p {
				case PermFileRead, PermFileWrite, PermCommandRun:
				default:
					return fmt.Errorf("client %s: unknown permission %q", cl.ID, p)
				}
			}
		}
	}
	return nil
}
