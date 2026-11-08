// Package orcatest provides a fake Orca CLI for tests. It keeps projects,
// their root workspace entries and terminals in <root>/state.json and
// answers in Orca's JSON format, with the behavior observed of the real CLI:
// an unknown project is repo_not_found, a project with saved details (a
// comment) is removed only with --force, removing a project leaves its
// folder on disk, and a terminal that is closed or was never there is
// terminal_handle_stale. The agent in every terminal is idle at its prompt
// and answers a prompt with the output lines "you: <prompt>" and
// "claude: echo: <prompt>".
package orcatest

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

const marker = "reach-fake-orca"

// Statuses are the workspace statuses the fake Orca knows.
var Statuses = []string{"todo", "in-progress", "in-review", "completed"}

// Command returns an Orca CLI command that runs the current test binary as a
// fake Orca keeping its state under root. The binary must call MaybeRun from
// the test selected by testName.
func Command(t testing.TB, testName, root string) []string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return []string{exe, "-test.run=^" + testName + "$", "--", marker, root}
}

type repo struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName"`
	Path        string `json:"path"`
	Kind        string `json:"kind"`
}

type worktree struct {
	ID              string `json:"id"`
	RepoID          string `json:"repoId"`
	Path            string `json:"path"`
	DisplayName     string `json:"displayName"`
	Comment         string `json:"comment"`
	WorkspaceStatus string `json:"workspaceStatus"`
	IsMainWorktree  bool   `json:"isMainWorktree"`
	LastActivityAt  int64  `json:"lastActivityAt"`
}

// terminal is a terminal whose agent echoes every prompt.
type terminal struct {
	Handle   string   `json:"handle"`
	Worktree string   `json:"worktreeId"`
	Command  string   `json:"command"`
	Open     bool     `json:"open"`
	Output   []string `json:"output"`
}

type state struct {
	Next      int                  `json:"next"`
	Repos     []*repo              `json:"repos"`
	Worktrees []*worktree          `json:"worktrees"`
	Terminals map[string]*terminal `json:"terminals"`
}

// MaybeRun acts as the fake Orca CLI and exits if the process was started by
// Command; otherwise it returns. A folder named "reach-fail" cannot be
// added. Every call is appended to <root>/calls.log.
func MaybeRun() {
	i := slices.Index(os.Args, "--")
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
	ok := func(result any) { out.Encode(map[string]any{"ok": true, "result": result}) }
	if len(args) < 2 {
		fail("unknown_command", strings.Join(args, " "))
	}
	unlock := lock(root)
	st := load(root)
	findTerminal := func() *terminal {
		h := flag(args, "--terminal")
		t := st.Terminals[h]
		if t == nil || !t.Open {
			fail("terminal_handle_stale", "terminal_handle_stale")
		}
		return t
	}
	findRepo := func(selector string) *repo {
		id, _ := strings.CutPrefix(selector, "id:")
		for _, r := range st.Repos {
			if r.ID == id {
				return r
			}
		}
		fail("repo_not_found", "repo_not_found")
		return nil
	}
	switch args[0] + " " + args[1] {
	case "repo add":
		path := flag(args, "--path")
		if filepath.Base(path) == "reach-fail" {
			fail("add_failed", "cannot add "+path)
		}
		if fi, err := os.Stat(path); err != nil || !fi.IsDir() {
			fail("path_not_found", path)
		}
		st.Next++
		r := &repo{ID: fmt.Sprintf("repo-%d", st.Next), DisplayName: filepath.Base(path), Path: path, Kind: flag(args, "--kind")}
		st.Repos = append(st.Repos, r)
		st.Worktrees = append(st.Worktrees, &worktree{ID: r.ID + "::" + path, RepoID: r.ID, Path: path, DisplayName: r.DisplayName,
			WorkspaceStatus: "in-progress", IsMainWorktree: true, LastActivityAt: time.Now().UnixMilli()})
		save(root, st)
		ok(map[string]any{"repo": r})
	case "repo list":
		ok(map[string]any{"repos": st.Repos})
	case "repo show":
		ok(map[string]any{"repo": findRepo(flag(args, "--repo"))})
	case "worktree list":
		list := st.Worktrees
		if sel := flag(args, "--repo"); sel != "" {
			r := findRepo(sel)
			list = slices.DeleteFunc(slices.Clone(list), func(w *worktree) bool { return w.RepoID != r.ID })
		}
		limit, _ := strconv.Atoi(flag(args, "--limit"))
		truncated := limit > 0 && len(list) > limit
		if truncated {
			list = list[:limit]
		}
		ok(map[string]any{"worktrees": list, "totalCount": len(list), "truncated": truncated})
	case "worktree set":
		id, _ := strings.CutPrefix(flag(args, "--worktree"), "id:")
		i := slices.IndexFunc(st.Worktrees, func(w *worktree) bool { return w.ID == id })
		if i < 0 {
			fail("selector_not_found", id)
		}
		w := st.Worktrees[i]
		if status := flag(args, "--workspace-status"); status != "" {
			if !slices.Contains(Statuses, status) {
				fail("invalid_workspace_status", status)
			}
			w.WorkspaceStatus = status
		}
		if has(args, "--comment") {
			w.Comment = flag(args, "--comment")
		}
		save(root, st)
		ok(map[string]any{"worktree": w})
	case "project setup-delete":
		r := findRepo("id:" + flag(args, "--setup"))
		saved := slices.ContainsFunc(st.Worktrees, func(w *worktree) bool { return w.RepoID == r.ID && w.Comment != "" })
		if saved && !has(args, "--force") {
			fail("runtime_error", "This project has saved details for 1 workspace. Re-run with --force to remove it anyway.")
		}
		st.Repos = slices.DeleteFunc(st.Repos, func(x *repo) bool { return x == r })
		st.Worktrees = slices.DeleteFunc(st.Worktrees, func(w *worktree) bool { return w.RepoID == r.ID })
		save(root, st)
		ok(map[string]any{"result": map[string]any{"repo": r}})
	case "terminal create":
		st.Next++
		t := &terminal{Handle: fmt.Sprintf("term_%d", st.Next), Worktree: strings.TrimPrefix(flag(args, "--worktree"), "id:"), Command: flag(args, "--command"), Open: true}
		st.Terminals[t.Handle] = t
		save(root, st)
		ok(map[string]any{"terminal": map[string]any{"handle": t.Handle, "title": flag(args, "--title")}})
	case "terminal list":
		list := []map[string]any{}
		for _, t := range st.Terminals {
			if t.Open {
				list = append(list, map[string]any{"handle": t.Handle, "worktreeId": t.Worktree, "connected": true, "agentIdentity": "claude"})
			}
		}
		ok(map[string]any{"terminals": list, "totalCount": len(list), "truncated": false})
	case "terminal show":
		t := findTerminal()
		ok(map[string]any{"terminal": map[string]any{"handle": t.Handle, "worktreeId": t.Worktree, "connected": true, "agentIdentity": "claude", "agentWait": nil}})
	case "terminal wait":
		t := findTerminal()
		if flag(args, "--for") != "tui-idle" || flag(args, "--timeout-ms") == "" {
			fail("invalid_argument", strings.Join(args, " "))
		}
		ok(map[string]any{"wait": map[string]any{"handle": t.Handle, "condition": "tui-idle", "satisfied": true, "status": "running", "exitCode": nil}})
	case "terminal read":
		t := findTerminal()
		// Orca's cursor counts completed lines; without one it reads the tail.
		if !has(args, "--cursor") {
			fail("unsupported", "the fake reads only by cursor")
		}
		from, _ := strconv.Atoi(flag(args, "--cursor"))
		from = min(from, len(t.Output))
		to := len(t.Output)
		if limit, _ := strconv.Atoi(flag(args, "--limit")); limit > 0 {
			to = min(to, from+limit)
		}
		ok(map[string]any{"terminal": map[string]any{"handle": t.Handle, "status": "running", "tail": t.Output[from:to], "truncated": false,
			"limited": to < len(t.Output), "oldestCursor": "0", "nextCursor": strconv.Itoa(to), "latestCursor": strconv.Itoa(len(t.Output)), "source": "stream"}})
	case "terminal send":
		t := findTerminal()
		text := flag(args, "--text")
		t.Output = append(t.Output, "you: "+text, "claude: echo: "+text)
		save(root, st)
		ok(map[string]any{"send": map[string]any{"handle": t.Handle, "accepted": true, "prompt": map[string]any{"stages": []string{"input_accepted", "turn_started"}}}})
	case "terminal close":
		findTerminal().Open = false
		save(root, st)
		ok(map[string]any{"closed": true})
	default:
		fail("unknown_command", strings.Join(args, " "))
	}
	unlock()
	os.Exit(0)
}

// Age makes every workspace entry's last activity d older, as if time passed.
func Age(t testing.TB, root string, d time.Duration) {
	t.Helper()
	unlock := lock(root)
	defer unlock()
	st := load(root)
	for _, w := range st.Worktrees {
		w.LastActivityAt -= d.Milliseconds()
	}
	save(root, st)
}

// Calls returns the Orca invocations recorded under root, one per line.
func Calls(t testing.TB, root string) string {
	t.Helper()
	b, _ := os.ReadFile(filepath.Join(root, "calls.log"))
	return string(b)
}

// lock serializes fake Orca processes sharing root.
func lock(root string) func() {
	f, err := os.OpenFile(filepath.Join(root, "state.lock"), os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		panic(err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		panic(err)
	}
	done := false
	return func() {
		if !done {
			done = true
			f.Close()
		}
	}
}

func load(root string) *state {
	st := &state{}
	if b, err := os.ReadFile(filepath.Join(root, "state.json")); err == nil {
		if err := json.Unmarshal(b, st); err != nil {
			panic(err)
		}
	}
	if st.Terminals == nil {
		st.Terminals = map[string]*terminal{}
	}
	return st
}

func save(root string, st *state) {
	b, err := json.Marshal(st)
	if err != nil {
		panic(err)
	}
	if err := os.WriteFile(filepath.Join(root, "state.json"), b, 0o644); err != nil {
		panic(err)
	}
}

func logCall(root string, args []string) {
	f, err := os.OpenFile(filepath.Join(root, "calls.log"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintln(f, strings.Join(args, " "))
}

// flag returns the value of --name value or --name=value.
func flag(args []string, name string) string {
	for i, a := range args {
		if v, ok := strings.CutPrefix(a, name+"="); ok {
			return v
		}
		if a == name && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func has(args []string, name string) bool {
	return slices.ContainsFunc(args, func(a string) bool { return a == name || strings.HasPrefix(a, name+"=") })
}
