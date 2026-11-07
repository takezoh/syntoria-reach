// Package workspace performs file operations confined to a workspace root.
//
// Paths are resolved with os.Root, which rejects ".." and symlinks that leave
// the root. Files on a different filesystem than the root (mount points
// inside the workspace) are rejected as well.
package workspace

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

var (
	// ErrInvalidPath: the path is not a clean relative path.
	ErrInvalidPath = errors.New("invalid path")
	// ErrOutside: the path resolves outside the workspace or onto another mount.
	ErrOutside = errors.New("path leaves the workspace")
	// ErrNotFound: the path does not exist.
	ErrNotFound = errors.New("not found")
	// ErrVersionConflict: the file's current version differs from the expected one.
	ErrVersionConflict = errors.New("version conflict")
	// ErrNotRegular: the path is not a regular file (or not a directory for List).
	ErrNotRegular = errors.New("unexpected file type")
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

// Entry is one item of a directory listing.
type Entry struct {
	Name    string    `json:"name"`
	Type    string    `json:"type"` // file, dir, symlink, other
	Size    int64     `json:"size"`
	ModTime time.Time `json:"mod_time"`
}

// List returns up to limit entries of the directory rel ("" or "." for the root).
func (w *FS) List(rel string, limit int) (entries []Entry, truncated bool, err error) {
	if rel == "" {
		rel = "."
	}
	if err := checkPath(rel); err != nil {
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
		return nil, false, ErrNotRegular
	}
	des, err := dir.ReadDir(limit + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, false, mapErr(err)
	}
	if len(des) > limit {
		des, truncated = des[:limit], true
	}
	for _, de := range des {
		e := Entry{Name: de.Name(), Type: typeOf(de.Type())}
		if info, err := de.Info(); err == nil {
			e.Size, e.ModTime = info.Size(), info.ModTime().UTC()
		}
		entries = append(entries, e)
	}
	return entries, truncated, nil
}

// ReadResult is a bounded slice of a file.
type ReadResult struct {
	Data    []byte `json:"data"`
	Size    int64  `json:"size"`
	Offset  int64  `json:"offset"`
	EOF     bool   `json:"eof"`
	Version string `json:"version"`
}

// Read returns at most limit bytes of rel starting at offset, plus the file's
// version (hash of the whole content) for a later conditional Write.
func (w *FS) Read(rel string, offset int64, limit int) (*ReadResult, error) {
	if err := checkPath(rel); err != nil {
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
	return &ReadResult{
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
	if err := checkPath(rel); err != nil {
		return "", err
	}
	w.writeMu.Lock()
	defer w.writeMu.Unlock()

	mode := fs.FileMode(0o644)
	f, err := w.openRegular(rel)
	switch {
	case errors.Is(err, ErrNotFound):
		if expectedVersion != "" {
			return "", ErrVersionConflict
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
			return "", ErrVersionConflict
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

	tmp := path.Join(dir, ".reach-tmp-"+randomSuffix())
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
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// open opens rel inside the root and rejects results on another filesystem.
func (w *FS) open(rel string) (*os.File, error) {
	f, err := w.root.Open(rel)
	if err != nil {
		return nil, mapErr(err)
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, mapErr(err)
	}
	if deviceOf(info) != w.dev {
		f.Close()
		return nil, ErrOutside
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
		return nil, ErrNotRegular
	}
	return f, nil
}

func checkPath(rel string) error {
	if rel == "." {
		return nil
	}
	if !filepath.IsLocal(rel) || strings.ContainsRune(rel, 0) {
		return fmt.Errorf("%w: %q", ErrInvalidPath, rel)
	}
	return nil
}

func mapErr(err error) error {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return ErrNotFound
	// os.Root does not export its escape error.
	case strings.Contains(err.Error(), "path escapes from parent"):
		return ErrOutside
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

func randomSuffix() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}
