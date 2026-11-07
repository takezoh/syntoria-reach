// Package orcatest provides a fake Orca CLI for tests. It creates worktrees
// as plain directories under a root and answers in Orca's JSON format.
package orcatest

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const marker = "reach-fake-orca"

// Command returns an Orca CLI command that runs the current test binary as a
// fake Orca keeping worktrees under root. The binary must call MaybeRun from
// the test selected by testName.
func Command(t testing.TB, testName, root string) []string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return []string{exe, "-test.run=^" + testName + "$", "--", marker, root}
}

// MaybeRun acts as the fake Orca CLI and exits if the process was started by
// Command; otherwise it returns. A worktree named "fail" cannot be created,
// and a worktree containing a file named "dirty" cannot be removed. Every
// call is appended to <root>/calls.log.
func MaybeRun() {
	i := indexOf(os.Args, "--")
	if i < 0 || i+2 >= len(os.Args) || os.Args[i+1] != marker {
		return
	}
	root, args := os.Args[i+2], os.Args[i+3:]
	logCall(root, args)
	out := json.NewEncoder(os.Stdout)
	fail := func(code, msg string) {
		out.Encode(map[string]any{"ok": false, "error": map[string]string{"code": code, "message": msg}})
		os.Exit(1)
	}
	if len(args) < 2 || args[0] != "worktree" {
		fail("unknown_command", strings.Join(args, " "))
	}
	switch args[1] {
	case "create":
		name := flag(args, "--name")
		if name == "fail" {
			fail("git_failed", "cannot create worktree")
		}
		path := filepath.Join(root, name)
		if err := os.Mkdir(path, 0o755); err != nil {
			fail("worktree_exists", err.Error())
		}
		out.Encode(map[string]any{"ok": true, "result": map[string]any{"worktree": map[string]any{
			"id": "repo::" + path, "path": path, "branch": "refs/heads/" + name,
		}}})
	case "rm":
		_, path, _ := strings.Cut(strings.TrimPrefix(flag(args, "--worktree"), "id:"), "::")
		if _, err := os.Stat(filepath.Join(path, "dirty")); err == nil {
			fail("worktree_dirty", "worktree has uncommitted changes")
		}
		if err := os.RemoveAll(path); err != nil {
			fail("remove_failed", err.Error())
		}
		out.Encode(map[string]any{"ok": true, "result": map[string]any{"removed": true}})
	default:
		fail("unknown_command", strings.Join(args, " "))
	}
	os.Exit(0)
}

// Calls returns the Orca invocations recorded under root, one per line.
func Calls(t testing.TB, root string) string {
	t.Helper()
	b, _ := os.ReadFile(filepath.Join(root, "calls.log"))
	return string(b)
}

func logCall(root string, args []string) {
	f, err := os.OpenFile(filepath.Join(root, "calls.log"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintln(f, strings.Join(args, " "))
}

func flag(args []string, name string) string {
	if i := indexOf(args, name); i >= 0 && i+1 < len(args) {
		return args[i+1]
	}
	return ""
}

func indexOf(args []string, v string) int {
	for i, a := range args {
		if a == v {
			return i
		}
	}
	return -1
}
