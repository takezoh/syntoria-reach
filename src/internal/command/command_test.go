package command

import (
	"context"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/takezoh/syntoria-reach/src/internal/core"
)

func TestRun(t *testing.T) {
	res, err := Run(context.Background(), core.CommandSpec{
		Executable: "/bin/sh", Args: []string{"-c", "echo out; echo err >&2; exit 3"},
		Dir: t.TempDir(), Timeout: 10 * time.Second, OutputLimit: 1024,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 3 || string(res.Stdout) != "out\n" || string(res.Stderr) != "err\n" || res.TimedOut {
		t.Fatalf("got %+v", res)
	}
}

func TestRunNoShellExpansionAndEnv(t *testing.T) {
	t.Setenv("REACH_TEST_SECRET", "s3cret")
	res, err := Run(context.Background(), core.CommandSpec{
		Executable: "/bin/echo", Args: []string{"$HOME", "*"},
		Dir: t.TempDir(), Timeout: 10 * time.Second, OutputLimit: 1024,
	})
	if err != nil || string(res.Stdout) != "$HOME *\n" {
		t.Fatalf("got %q, %v", res.Stdout, err)
	}
	res, _ = Run(context.Background(), core.CommandSpec{
		Executable: "/usr/bin/env", Dir: t.TempDir(), Timeout: 10 * time.Second, OutputLimit: 4096,
	})
	if s := string(res.Stdout); strings.Contains(s, "REACH_TEST_SECRET") {
		t.Fatalf("daemon environment leaked: %s", s)
	}
}

// Commands and jobs (both use Env(nil)) get exactly PATH, LANG and HOME, and
// none of the daemon's environment, such as a token handed to a Controller.
func TestRunEnvIsOnlyPathLangHome(t *testing.T) {
	t.Setenv("REACH_TOKEN", "token-sentinel")
	res, err := Run(context.Background(), core.CommandSpec{
		Executable: "/usr/bin/env", Dir: t.TempDir(), Timeout: 10 * time.Second, OutputLimit: 4096,
	})
	if err != nil {
		t.Fatal(err)
	}
	out := string(res.Stdout)
	if strings.Contains(out, "token-sentinel") {
		t.Fatalf("daemon environment leaked: %s", out)
	}
	var names []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		name, _, _ := strings.Cut(line, "=")
		names = append(names, name)
	}
	sort.Strings(names)
	if strings.Join(names, ",") != "HOME,LANG,PATH" {
		t.Fatalf("got environment %q, want only HOME, LANG and PATH", names)
	}
}

func TestRunTimeoutKillsGroupAndTruncates(t *testing.T) {
	start := time.Now()
	res, err := Run(context.Background(), core.CommandSpec{
		Executable: "/bin/sh", Args: []string{"-c", "head -c 5000 /dev/zero; sleep 30 & wait"},
		Dir: t.TempDir(), Timeout: 300 * time.Millisecond, OutputLimit: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.TimedOut || !res.Truncated || len(res.Stdout) != 100 {
		t.Fatalf("got timed_out=%v truncated=%v len=%d", res.TimedOut, res.Truncated, len(res.Stdout))
	}
	if time.Since(start) > 10*time.Second {
		t.Fatal("timeout did not kill the process group")
	}
}
