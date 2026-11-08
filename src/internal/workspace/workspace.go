// Package workspace performs file operations confined to a workspace root.
//
// Paths are resolved with os.Root, which rejects ".." and symlinks that leave
// the root. Files on a different filesystem than the root (mount points
// inside the workspace) are rejected as well.
package workspace

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/takezoh/syntoria-reach/src/internal/core"
	"github.com/takezoh/syntoria-reach/src/internal/ids"
)

// FS is an open workspace root.
type FS struct {
	root    *os.Root
	dev     uint64
	writeMu sync.Mutex
}

// Open opens the workspace rooted at dir.
func Open(dir string) (*FS, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	info, err := root.Stat(".")
	if err != nil {
		root.Close()
		return nil, err
	}
	return &FS{root: root, dev: deviceOf(info)}, nil
}

// Close releases the root.
func (w *FS) Close() error { return w.root.Close() }

// List returns up to limit entries of the directory rel ("" or "." for the root).
func (w *FS) List(rel string, limit int) (entries []core.FileEntry, truncated bool, err error) {
	if rel == "" {
		rel = "."
	}
	if err := core.CheckPath(rel); err != nil {
		return nil, false, err
	}
	dir, err := w.open(rel)
	if err != nil {
		return nil, false, err
	}
	defer dir.Close()
	if info, err := dir.Stat(); err != nil {
		return nil, false, mapErr(err)
	} else if !info.IsDir() {
		return nil, false, core.ErrNotRegular
	}
	des, err := dir.ReadDir(limit + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, false, mapErr(err)
	}
	if len(des) > limit {
		des, truncated = des[:limit], true
	}
	for _, de := range des {
		e := core.FileEntry{Name: de.Name(), Type: typeOf(de.Type())}
		if info, err := de.Info(); err == nil {
			e.Size, e.ModTime = info.Size(), info.ModTime().UTC()
		}
		entries = append(entries, e)
	}
	return entries, truncated, nil
}

// Read returns at most limit bytes of rel starting at offset, plus the file's
// version (hash of the whole content) for a later conditional Write.
func (w *FS) Read(rel string, offset int64, limit int) (*core.FileChunk, error) {
	if err := core.CheckPath(rel); err != nil {
		return nil, err
	}
	f, err := w.openRegular(rel)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, mapErr(err)
	}
	version, err := hashReader(f)
	if err != nil {
		return nil, mapErr(err)
	}
	buf := make([]byte, limit)
	n, err := f.ReadAt(buf, offset)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, mapErr(err)
	}
	return &core.FileChunk{
		Data:    buf[:n],
		Size:    info.Size(),
		Offset:  offset,
		EOF:     offset+int64(n) >= info.Size(),
		Version: version,
	}, nil
}

// Write replaces rel with data atomically. expectedVersion "" requires that
// the file does not exist; otherwise it must equal the current version.
// Missing parent directories are created. It returns the new version.
func (w *FS) Write(rel string, data []byte, expectedVersion string) (string, error) {
	if err := core.CheckPath(rel); err != nil {
		return "", err
	}
	w.writeMu.Lock()
	defer w.writeMu.Unlock()

	mode := fs.FileMode(0o644)
	f, err := w.openRegular(rel)
	switch {
	case errors.Is(err, core.ErrNotFound):
		if expectedVersion != "" {
			return "", core.ErrVersionConflict
		}
	case err != nil:
		return "", err
	default:
		info, statErr := f.Stat()
		current, hashErr := hashReader(f)
		f.Close()
		if err := errors.Join(statErr, hashErr); err != nil {
			return "", mapErr(err)
		}
		if expectedVersion != current {
			return "", core.ErrVersionConflict
		}
		mode = info.Mode().Perm()
	}

	dir := path.Dir(filepath.ToSlash(rel))
	if err := w.root.MkdirAll(dir, 0o755); err != nil {
		return "", mapErr(err)
	}
	if d, err := w.open(dir); err != nil {
		return "", err
	} else {
		d.Close()
	}

	tmp := path.Join(dir, ids.New(".reach-tmp-"))
	tf, err := w.root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return "", mapErr(err)
	}
	_, err = tf.Write(data)
	if err == nil {
		err = tf.Sync()
	}
	if cerr := tf.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = w.root.Rename(tmp, rel)
	}
	if err != nil {
		w.root.Remove(tmp)
		return "", mapErr(err)
	}
	return core.FileVersion(data), nil
}

// open opens rel inside the root. It accepts only regular files and
// directories on the root's filesystem. O_NONBLOCK keeps a FIFO or device in
// the workspace from blocking the open.
func (w *FS) open(rel string) (*os.File, error) {
	f, err := w.root.OpenFile(rel, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, mapErr(err)
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, mapErr(err)
	}
	if !info.Mode().IsRegular() && !info.IsDir() {
		f.Close()
		return nil, core.ErrNotRegular
	}
	if deviceOf(info) != w.dev {
		f.Close()
		return nil, core.ErrOutside
	}
	return f, nil
}

func (w *FS) openRegular(rel string) (*os.File, error) {
	f, err := w.open(rel)
	if err != nil {
		return nil, err
	}
	if info, err := f.Stat(); err != nil || !info.Mode().IsRegular() {
		f.Close()
		if err != nil {
			return nil, mapErr(err)
		}
		return nil, core.ErrNotRegular
	}
	return f, nil
}

func mapErr(err error) error {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return core.ErrNotFound
	// os.Root does not export its escape error.
	case strings.Contains(err.Error(), "path escapes from parent"):
		return core.ErrOutside
	}
	return err
}

func hashReader(f *os.File) (string, error) {
	h := sha256.New()
	if _, err := io.Copy(h, io.NewSectionReader(f, 0, 1<<62)); err != nil {
		return "", err
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

func deviceOf(info fs.FileInfo) uint64 {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Dev)
	}
	return 0
}

func typeOf(m fs.FileMode) string {
	switch {
	case m.IsRegular():
		return "file"
	case m.IsDir():
		return "dir"
	case m&fs.ModeSymlink != 0:
		return "symlink"
	}
	return "other"
}

// Folders creates and removes workspace folders.
type Folders struct{}

// Make creates the folder path, which must not exist yet, and its parents.
func (Folders) Make(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.Mkdir(path, 0o755)
}

// Remove removes the folder path and everything in it. Symlinks, including
// path itself, are removed rather than followed.
func (Folders) Remove(path string) error { return os.RemoveAll(path) }

// List returns the names of the folders in dir, none when dir does not
// exist. A symlink is not a folder.
func (Folders) List(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	return names, nil
}

// ChangedSince reports whether the folder path, or anything in it, was
// modified at or after t. Symlinks are not followed.
func (Folders) ChangedSince(path string, t time.Time) (bool, error) {
	changed := false
	err := filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !info.ModTime().Before(t) {
			changed = true
			return fs.SkipAll
		}
		return nil
	})
	return changed, err
}
