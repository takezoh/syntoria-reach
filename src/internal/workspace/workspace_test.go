package workspace

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/takezoh/syntoria-reach/src/internal/core"
	"github.com/takezoh/syntoria-reach/src/internal/service"
	"github.com/takezoh/syntoria-reach/src/internal/service/servicetest"
)

func setup(t *testing.T) (*FS, string, string) {
	t.Helper()
	base := t.TempDir()
	root := filepath.Join(base, "ws")
	outside := filepath.Join(base, "outside")
	for _, d := range []string{root, outside, filepath.Join(root, "sub")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write(t, filepath.Join(root, "a.txt"), "hello")
	write(t, filepath.Join(outside, "secret"), "secret")
	if err := os.Symlink(filepath.Join(outside, "secret"), filepath.Join(root, "link-out")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "dir-out")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("a.txt", filepath.Join(root, "link-in")); err != nil {
		t.Fatal(err)
	}
	w, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { w.Close() })
	return w, root, outside
}

func write(t *testing.T, path, s string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestReadRejectsEscapes(t *testing.T) {
	w, _, outside := setup(t)
	cases := map[string]error{
		"../outside/secret":              core.ErrInvalidPath,
		filepath.Join(outside, "secret"): core.ErrInvalidPath,
		"link-out":                       core.ErrOutside,
		"dir-out/secret":                 core.ErrOutside,
		"missing":                        core.ErrNotFound,
		"sub":                            core.ErrNotRegular,
	}
	for p, want := range cases {
		if _, err := w.Read(p, 0, 10); !errors.Is(err, want) {
			t.Errorf("Read(%q) err = %v, want %v", p, err, want)
		}
	}
}

func TestReadBoundedAndSymlinkInside(t *testing.T) {
	w, _, _ := setup(t)
	r, err := w.Read("link-in", 1, 3)
	if err != nil {
		t.Fatal(err)
	}
	if string(r.Data) != "ell" || r.Size != 5 || r.EOF {
		t.Fatalf("got %+v", r)
	}
	r, err = w.Read("a.txt", 3, 10)
	if err != nil || string(r.Data) != "lo" || !r.EOF {
		t.Fatalf("got %+v, %v", r, err)
	}
}

func TestWriteConditional(t *testing.T) {
	w, root, outside := setup(t)

	if _, err := w.Write("a.txt", []byte("x"), ""); !errors.Is(err, core.ErrVersionConflict) {
		t.Fatalf("create-only on existing file: %v", err)
	}
	r, _ := w.Read("a.txt", 0, 100)
	v, err := w.Write("a.txt", []byte("world"), r.Version)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "a.txt")); string(b) != "world" {
		t.Fatalf("content = %q", b)
	}
	if _, err := w.Write("a.txt", []byte("again"), r.Version); !errors.Is(err, core.ErrVersionConflict) {
		t.Fatalf("stale version: %v", err)
	}
	if r2, _ := w.Read("a.txt", 0, 100); r2.Version != v {
		t.Fatalf("version %s != %s", r2.Version, v)
	}

	if _, err := w.Write("new/dir/b.txt", []byte("b"), ""); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write("dir-out/new", []byte("x"), ""); !errors.Is(err, core.ErrOutside) {
		t.Fatalf("write through symlinked dir: %v", err)
	}
	if _, err := w.Write("link-out", []byte("x"), "sha256:x"); !errors.Is(err, core.ErrOutside) {
		t.Fatalf("write through symlinked file: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(outside, "secret")); string(b) != "secret" {
		t.Fatal("outside file modified")
	}
	if _, err := os.Stat(filepath.Join(outside, "new")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("file created outside workspace")
	}
	entries, _, _ := w.List("", 100)
	for _, e := range entries {
		if strings.HasPrefix(e.Name, ".reach-tmp") {
			t.Fatalf("temp file left: %s", e.Name)
		}
	}
}

func TestList(t *testing.T) {
	w, _, _ := setup(t)
	entries, truncated, err := w.List(".", 2)
	if err != nil || len(entries) != 2 || !truncated {
		t.Fatalf("entries=%v truncated=%v err=%v", entries, truncated, err)
	}
	if _, _, err := w.List("dir-out", 10); !errors.Is(err, core.ErrOutside) {
		t.Fatalf("list symlinked dir: %v", err)
	}
	if _, _, err := w.List("a.txt", 10); !errors.Is(err, core.ErrNotRegular) {
		t.Fatalf("list file: %v", err)
	}
}

func TestFIFODoesNotBlock(t *testing.T) {
	w, root, _ := setup(t)
	if err := syscall.Mkfifo(filepath.Join(root, "fifo"), 0o644); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := w.Read("fifo", 0, 10); !errors.Is(err, core.ErrNotRegular) {
			t.Errorf("Read(fifo) err = %v", err)
		}
		if _, err := w.Write("fifo", []byte("x"), "sha256:x"); !errors.Is(err, core.ErrNotRegular) {
			t.Errorf("Write(fifo) err = %v", err)
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("opening a FIFO blocked")
	}
}

func TestFilesContract(t *testing.T) {
	servicetest.FilesContract(t, func(t *testing.T) service.Files {
		f, err := Open(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { f.Close() })
		return f
	})
}

func TestFolders(t *testing.T) {
	base := filepath.Join(t.TempDir(), "workspaces")
	path := filepath.Join(base, "reach-ws")
	if err := (Folders{}).Make(path); err != nil {
		t.Fatal(err)
	}
	if err := (Folders{}).Make(path); err == nil {
		t.Fatal("an existing folder was made again")
	}
	// Removing goes through everything in the folder but never follows a
	// symlink out of it.
	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "keep"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(path, "f"), []byte("x"), 0o644)
	os.Symlink(outside, filepath.Join(path, "link"))
	if err := (Folders{}).Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("folder still exists")
	}
	if _, err := os.Stat(filepath.Join(outside, "keep")); err != nil {
		t.Fatal("removal followed a symlink")
	}
}

// Cleanup finds workspace folders and tells whether anything in one changed
// lately, never through a symlink out of it.
func TestFoldersListAndChanges(t *testing.T) {
	base := filepath.Join(t.TempDir(), "workspaces")
	if names, err := (Folders{}).List(base); err != nil || len(names) != 0 {
		t.Fatalf("list a missing folder: %q %v", names, err)
	}
	path := filepath.Join(base, "reach-ws")
	if err := (Folders{}).Make(path); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	target := filepath.Join(outside, "target")
	os.WriteFile(target, []byte("x"), 0o644)
	os.WriteFile(filepath.Join(base, "file"), nil, 0o644)
	os.Symlink(outside, filepath.Join(base, "reach-link"))
	if names, err := (Folders{}).List(base); err != nil || !slices.Equal(names, []string{"reach-ws"}) {
		t.Fatalf("list = %q %v", names, err)
	}

	nested := filepath.Join(path, "a", "b", "f")
	os.MkdirAll(filepath.Dir(nested), 0o755)
	os.WriteFile(nested, []byte("x"), 0o644)
	os.Symlink(target, filepath.Join(path, "link"))
	old := time.Now().Add(-48 * time.Hour)
	for _, p := range []string{nested, filepath.Dir(nested), filepath.Join(path, "a"), path} {
		os.Chtimes(p, old, old)
	}
	since := time.Now() // after everything in the folder was written
	os.Chtimes(target, since.Add(time.Hour), since.Add(time.Hour))
	if changed, err := (Folders{}).ChangedSince(path, since); err != nil || changed {
		t.Fatalf("only the symlink's target changed: %v %v", changed, err)
	}
	os.Chtimes(nested, since, since)
	if changed, err := (Folders{}).ChangedSince(path, since); err != nil || !changed {
		t.Fatalf("a file deep in the folder changed: %v %v", changed, err)
	}
	if _, err := (Folders{}).ChangedSince(filepath.Join(base, "reach-gone"), since); err == nil {
		t.Fatal("a missing folder has no changes to tell")
	}
}
