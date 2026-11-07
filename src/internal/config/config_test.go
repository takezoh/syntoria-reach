package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
  "workspaces": [{"id": "ws", "root": "/srv/ws", "commands": ["/usr/bin/make"]}],
  "clients": [{"id": "a", "token": "0123456789abcdef0123456789abcdef",
               "grants": [{"workspace": "ws", "allow": ["file.read", "command.run"]}]}]
}`

func TestLoad(t *testing.T) {
	c, err := Load(writeConfig(t, 0o600, valid))
	if err != nil {
		t.Fatal(err)
	}
	if c.Workspaces[0].Commands[0] != "/usr/bin/make" || c.Clients[0].Grants[0].Allow[1] != PermCommandRun {
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
		"unknown ws":       strings.Replace(valid, `"workspace": "ws"`, `"workspace": "x"`, 1),
		"short token":      strings.Replace(valid, `0123456789abcdef0123456789abcdef`, `short`, 1),
		"relative root":    strings.Replace(valid, `"/srv/ws"`, `"srv"`, 1),
	}
	for name, body := range bad {
		if _, err := Load(writeConfig(t, 0o600, body)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
