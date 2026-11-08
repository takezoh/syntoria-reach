package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/takezoh/syntoria-reach/src/internal/core"
)

func writeConfig(t *testing.T, mode os.FileMode, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	return p
}

const valid = `{
  "socket": "/run/reach.sock",
  "database": "/var/reach.db",
  "orca": ["/usr/bin/orca-ide"],
  "commands": ["/usr/bin/make"],
  "clients": [{"id": "a", "token": "0123456789abcdef0123456789abcdef", "allow": ["workspace.create", "file.read", "command.run"]}]
}`

func TestLoad(t *testing.T) {
	c, err := Load(writeConfig(t, 0o600, valid))
	if err != nil {
		t.Fatal(err)
	}
	if c.Commands[0] != "/usr/bin/make" || c.Clients[0].Allow[2] != core.PermCommandRun || c.Workspaces != "/var/workspaces" {
		t.Fatalf("got %+v", c)
	}
}

func TestLoadRejects(t *testing.T) {
	if _, err := Load(writeConfig(t, 0o644, valid)); err == nil {
		t.Error("group/other readable config accepted")
	}
	bad := map[string]string{
		"relative command":    strings.Replace(valid, `"/usr/bin/make"`, `"make"`, 1),
		"unknown perm":        strings.Replace(valid, `"command.run"`, `"root"`, 1),
		"missing orca":        strings.Replace(valid, `"orca": ["/usr/bin/orca-ide"],`, ``, 1),
		"short token":         strings.Replace(valid, `0123456789abcdef0123456789abcdef`, `short`, 1),
		"relative workspaces": strings.Replace(valid, `"socket"`, `"workspaces": "ws", "socket"`, 1),
		// Keys of the earlier format are errors, not silently ignored.
		"projects": strings.Replace(valid, `"socket"`, `"projects": [], "socket"`, 1),
		"grants":   strings.Replace(valid, `"allow"`, `"grants": [], "allow"`, 1),
	}
	for name, body := range bad {
		if _, err := Load(writeConfig(t, 0o600, body)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestRemote(t *testing.T) {
	withRemote := func(remote string) string {
		return strings.Replace(valid, `"socket"`, `"remote": `+remote+`, "socket"`, 1)
	}
	const token = `"node-token-0123456789abcdef0123456789"`
	for _, u := range []string{"wss://reach-api.example.test/connect", "ws://127.0.0.1:8787/connect", "ws://localhost:8787/connect", "ws://[::1]:8787/connect"} {
		c, err := Load(writeConfig(t, 0o600, withRemote(`{"url": "`+u+`", "token": `+token+`, "client": "a"}`)))
		if err != nil || c.Remote.Client != "a" {
			t.Errorf("%s: %+v %v", u, c, err)
		}
	}
	bad := map[string]string{
		"plain ws to a remote host": `{"url": "ws://reach.example.com/connect", "token": ` + token + `, "client": "a"}`,
		"https":                     `{"url": "https://reach.example.com/connect", "token": ` + token + `, "client": "a"}`,
		"relative url":              `{"url": "/connect", "token": ` + token + `, "client": "a"}`,
		"short token":               `{"url": "wss://reach.example.com/connect", "token": "short", "client": "a"}`,
		"unknown client":            `{"url": "wss://reach.example.com/connect", "token": ` + token + `, "client": "nobody"}`,
	}
	for name, remote := range bad {
		if _, err := Load(writeConfig(t, 0o600, withRemote(remote))); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestApplyDefaults(t *testing.T) {
	env := map[string]string{"XDG_RUNTIME_DIR": "/run/user/1"}
	c := &Config{}
	if err := c.ApplyDefaults(func(k string) string { return env[k] }, "/home/u"); err != nil {
		t.Fatal(err)
	}
	if c.Socket != "/run/user/1/reach/reach.sock" || c.Database != "/home/u/.local/state/reach/reach.db" || c.Workspaces != "/home/u/.local/state/reach/workspaces" {
		t.Fatalf("got socket=%s database=%s workspaces=%s", c.Socket, c.Database, c.Workspaces)
	}
	env["XDG_STATE_HOME"] = "/state"
	c = &Config{Socket: "/s.sock"}
	if err := c.ApplyDefaults(func(k string) string { return env[k] }, ""); err != nil || c.Socket != "/s.sock" || c.Database != "/state/reach/reach.db" {
		t.Fatalf("got %+v err=%v", c, err)
	}
	if err := (&Config{}).ApplyDefaults(func(string) string { return "" }, "/home/u"); err == nil {
		t.Fatal("missing XDG_RUNTIME_DIR accepted")
	}
	if err := (&Config{Socket: "/s.sock"}).ApplyDefaults(func(string) string { return "" }, ""); err == nil {
		t.Fatal("unknown home accepted")
	}
}
