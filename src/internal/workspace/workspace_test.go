package workspace

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
		"../outside/secret":              ErrInvalidPath,
		filepath.Join(outside, "secret"): ErrInvalidPath,
		"link-out":                       ErrOutside,
		"dir-out/secret":                 ErrOutside,
		"missing":                        ErrNotFound,
		"sub":                            ErrNotRegular,
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

	if _, err := w.Write("a.txt", []byte("x"), ""); !errors.Is(err, ErrVersionConflict) {
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
	if _, err := w.Write("a.txt", []byte("again"), r.Version); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("stale version: %v", err)
	}
	if r2, _ := w.Read("a.txt", 0, 100); r2.Version != v {
		t.Fatalf("version %s != %s", r2.Version, v)
	}

	if _, err := w.Write("new/dir/b.txt", []byte("b"), ""); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write("dir-out/new", []byte("x"), ""); !errors.Is(err, ErrOutside) {
		t.Fatalf("write through symlinked dir: %v", err)
	}
	if _, err := w.Write("link-out", []byte("x"), "sha256:x"); !errors.Is(err, ErrOutside) {
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
	if _, _, err := w.List("dir-out", 10); !errors.Is(err, ErrOutside) {
		t.Fatalf("list symlinked dir: %v", err)
	}
	if _, _, err := w.List("a.txt", 10); !errors.Is(err, ErrNotRegular) {
		t.Fatalf("list file: %v", err)
	}
}
