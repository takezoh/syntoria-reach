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
  "projects": [{"id": "proj", "repo": "/srv/repo", "commands": ["/usr/bin/make"]}],
  "clients": [{"id": "a", "token": "0123456789abcdef0123456789abcdef",
               "grants": [{"project": "proj", "allow": ["workspace.create", "file.read", "command.run"]}]}]
}`

func TestLoad(t *testing.T) {
	c, err := Load(writeConfig(t, 0o600, valid))
	if err != nil {
		t.Fatal(err)
	}
	if c.Projects[0].Commands[0] != "/usr/bin/make" || c.Clients[0].Grants[0].Allow[2] != core.PermCommandRun {
		t.Fatalf("got %+v", c)
	}
}

func TestLoadRejects(t *testing.T) {
	if _, err := Load(writeConfig(t, 0o644, valid)); err == nil {
		t.Error("group/other readable config accepted")
	}
	bad := map[string]string{
		"relative command": strings.Replace(valid, `"/usr/bin/make"`, `"make"`, 1),
		"unknown perm":     strings.Replace(valid, `"command.run"`, `"root"`, 1),
		"unknown project":  strings.Replace(valid, `"project": "proj"`, `"project": "x"`, 1),
		"missing orca":     strings.Replace(valid, `"orca": ["/usr/bin/orca-ide"],`, ``, 1),
		"short token":      strings.Replace(valid, `0123456789abcdef0123456789abcdef`, `short`, 1),
		"relative repo":    strings.Replace(valid, `"/srv/repo"`, `"srv"`, 1),
	}
	for name, body := range bad {
		if _, err := Load(writeConfig(t, 0o600, body)); err == nil {
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
	if c.Socket != "/run/user/1/reach/reach.sock" || c.Database != "/home/u/.local/state/reach/reach.db" {
		t.Fatalf("got socket=%s database=%s", c.Socket, c.Database)
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
