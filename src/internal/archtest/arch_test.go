// Package archtest checks the dependency rules of the module: which internal
// packages each layer may import, and which side effects the pure core and
// the service may reach without going through a port. Test files are not
// checked; tests may use fakes and helpers freely.
package archtest

import (
	"errors"
	"fmt"
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

const module = "github.com/takezoh/syntoria-reach/src"

// rule is what a package may depend on. Internal lists the internal
// packages (relative to the module) it may import; nil means any. Std lists
// standard library packages it must not import, and Calls the package-level
// functions it must not use, by import path.
type rule struct {
	Internal []string
	Std      []string
	Calls    map[string][]string
}

// sideEffects are the standard library packages that reach the operating
// system, the network, the database driver or randomness.
var sideEffects = []string{
	"os", "os/exec", "os/signal", "net", "net/http", "database/sql", "syscall",
	"crypto/rand", "math/rand", "math/rand/v2", "io/fs", "log", "log/slog",
}

// clock is the part of package time that reads or waits on the clock.
var clock = map[string][]string{"time": {"Now", "Since", "Until", "Sleep", "After", "AfterFunc", "Tick", "NewTimer", "NewTicker"}}

var adapter = rule{Internal: []string{"internal/core", "internal/command", "internal/ids"}}

// decider is the rule of a package that decides and keeps every side
// effect behind ports. It may log.
func decider(internal ...string) rule {
	return rule{
		Internal: append([]string{"internal/core"}, internal...),
		Std:      slices.DeleteFunc(slices.Clone(sideEffects), func(p string) bool { return p == "log/slog" }),
		Calls:    clock,
	}
}

// rules classifies every package of the module. A new package must be
// added here before it builds into a passing test run.
var rules = map[string]rule{
	"internal/core":   {Internal: []string{}, Std: sideEffects, Calls: clock},
	"internal/ids":    {Internal: []string{}},
	"internal/config": {Internal: []string{"internal/core"}},
	// The service decides and records; every side effect goes through a
	// port.
	"internal/service":             decider("internal/config"),
	"internal/service/servicetest": {Internal: []string{"internal/core", "internal/service"}},
	// The workspace backends are the service's Workspaces port: they decide
	// what a workspace and a session are on Orca and on a node without it,
	// and reach Orca, folders, records and agent processes through ports
	// of their own.
	"internal/orcaws":  decider(),
	"internal/localws": decider(),
	// The method table shared by the local API and the remote connector.
	"internal/api":      {Internal: []string{"internal/core", "internal/service"}},
	"internal/localapi": {Internal: []string{"internal/core", "internal/service", "internal/api"}},
	// The connector is transport only: it hands requests to a handler and
	// knows nothing of the service.
	"internal/remote":        {Internal: []string{"internal/core"}},
	"internal/store":         adapter,
	"internal/workspace":     adapter,
	"internal/command":       {Internal: []string{"internal/core"}},
	"internal/job":           adapter,
	"internal/orca":          adapter,
	"internal/orca/orcatest": {Internal: []string{}},
	// Agent processes of local workspaces.
	"internal/agent":           adapter,
	"internal/agent/agenttest": {Internal: []string{}},
	"internal/archtest":        {Internal: []string{}},
	"cmd/reach":                {}, // the composition root wires everything
}

func TestDependencyRules(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		if name := d.Name(); path != root && (name == "testdata" || strings.HasPrefix(name, ".")) {
			return filepath.SkipDir
		}
		pkg, err := build.ImportDir(path, 0)
		var noGo *build.NoGoError
		if errors.As(err, &noGo) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		r, ok := rules[rel]
		if !ok {
			t.Errorf("%s has no dependency rule; classify it in rules", rel)
			return nil
		}
		seen++
		for _, v := range checkImports(rel, pkg.Imports, r) {
			t.Error(v)
		}
		for _, file := range pkg.GoFiles {
			src, err := os.ReadFile(filepath.Join(path, file))
			if err != nil {
				return err
			}
			vs, err := checkCalls(rel+"/"+file, src, r.Calls)
			if err != nil {
				return err
			}
			for _, v := range vs {
				t.Error(v)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// Guard against checking nothing (a wrong root or a broken walk).
	if seen != len(rules) {
		t.Errorf("checked %d packages, rules name %d", seen, len(rules))
	}
}

// checkImports returns the imports of pkg that r forbids.
func checkImports(pkg string, imports []string, r rule) []string {
	var out []string
	for _, imp := range imports {
		if rel, ok := strings.CutPrefix(imp, module+"/"); ok {
			if r.Internal != nil && !slices.Contains(r.Internal, rel) {
				out = append(out, fmt.Sprintf("%s must not import %s", pkg, rel))
			}
			continue
		}
		if slices.Contains(r.Std, imp) {
			out = append(out, fmt.Sprintf("%s must not import %s", pkg, imp))
		}
	}
	return out
}

// checkCalls returns the uses in src of the functions calls forbids, by
// import path. It resolves import names and aliases; an identifier that
// shadows the import is not a use.
func checkCalls(name string, src []byte, calls map[string][]string) ([]string, error) {
	if len(calls) == 0 {
		return nil, nil
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, name, src, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	names := map[string]string{} // local name -> import path
	for _, imp := range f.Imports {
		path, _ := strconv.Unquote(imp.Path.Value)
		if _, ok := calls[path]; !ok {
			continue
		}
		local := path[strings.LastIndex(path, "/")+1:]
		if imp.Name != nil {
			local = imp.Name.Name
		}
		names[local] = path
	}
	var out []string
	ast.Inspect(f, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		x, ok := sel.X.(*ast.Ident)
		if !ok {
			return true
		}
		path, ok := names[x.Name]
		if ok && slices.Contains(calls[path], sel.Sel.Name) && !shadowed(f, x) {
			out = append(out, fmt.Sprintf("%s: %s.%s reaches %s directly; use a port", fset.Position(sel.Pos()), path, sel.Sel.Name, path))
		}
		return true
	})
	return out, nil
}

// shadowed reports whether ident is declared in the file as something other
// than the import (a parameter, variable or field), conservatively by name
// within the enclosing function.
func shadowed(f *ast.File, ident *ast.Ident) bool {
	found := false
	ast.Inspect(f, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || found {
			return !found
		}
		if ident.Pos() < fn.Pos() || ident.Pos() > fn.End() {
			return false
		}
		ast.Inspect(fn, func(n ast.Node) bool {
			switch d := n.(type) {
			case *ast.Field:
				for _, nm := range d.Names {
					found = found || nm.Name == ident.Name
				}
			case *ast.AssignStmt:
				if d.Tok == token.DEFINE {
					for _, l := range d.Lhs {
						if id, ok := l.(*ast.Ident); ok && id.Name == ident.Name && id.Pos() < ident.Pos() {
							found = true
						}
					}
				}
			case *ast.ValueSpec:
				for _, nm := range d.Names {
					found = found || nm.Name == ident.Name
				}
			}
			return true
		})
		return false
	})
	return found
}

// The checkers reject the representative ways to reach a side effect and
// accept the allowed uses.
func TestCheckersRejectViolations(t *testing.T) {
	r := rules["internal/service"]
	if v := checkImports("internal/service", []string{module + "/internal/store", "os", "log/slog", module + "/internal/core"}, r); len(v) != 2 ||
		!strings.Contains(v[0], "internal/store") || !strings.Contains(v[1], " os") {
		t.Errorf("imports: %q", v)
	}
	if v := checkImports("cmd/reach", []string{module + "/internal/store", "os"}, rules["cmd/reach"]); len(v) != 0 {
		t.Errorf("composition root: %q", v)
	}

	cases := map[string]int{
		// Direct call, method value and alias are uses.
		`package p; import "time"; func f() { _ = time.Now() }`:                          1,
		`package p; import "time"; var now = time.Now`:                                   1,
		`package p; import clock "time"; func f() { clock.Sleep(1) }`:                    1,
		`package p; import "time"; func f() { <-time.After(1); time.AfterFunc(1, nil) }`: 2,
		// Types, constants and pure functions of time are allowed.
		`package p; import "time"; func f(d time.Duration) time.Time { return time.Unix(0, 0).Add(d * time.Second) }`: 0,
		// A parameter or variable named like the import is not the import.
		`package p; import "time"; type c struct{ Now func() }; func f(time c) { time.Now() }`:        0,
		`package p; import "time"; type c struct{ Now func() }; func f() { time := c{}; time.Now() }`: 0,
		// Without the import, time.Now is someone else's.
		`package p; type c struct{ Now func() }; var time c; func f() { time.Now() }`: 0,
	}
	for src, want := range cases {
		v, err := checkCalls("x.go", []byte(src), clock)
		if err != nil {
			t.Fatal(err)
		}
		if len(v) != want {
			t.Errorf("%s: %d violations %q, want %d", src, len(v), v, want)
		}
	}
}
