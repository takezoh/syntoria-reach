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
	if c.Projects[0].Commands[0] != "/usr/bin/make" || c.Clients[0].Grants[0].Allow[2] != PermCommandRun {
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
