// Package fsguard confines all input access to a caller-supplied root and
// removes partially written outputs on failure.
package fsguard

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// ErrEscape is returned when a path resolves outside the input root.
var ErrEscape = errors.New("path escapes input root")

// Root is a confined input directory.
type Root struct {
	// Dir is the canonical absolute root directory.
	Dir string
	// FollowSymlinks allows symlinks that resolve inside Dir. It never allows
	// escapes; it only controls whether such links are accepted at all.
	FollowSymlinks bool
}

// OpenRoot canonicalises dir and checks that it is a directory.
func OpenRoot(dir string) (*Root, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(real)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", real)
	}
	return &Root{Dir: real}, nil
}

// Resolve converts a user-supplied path (absolute or root-relative) into a
// canonical path guaranteed to live under Root.Dir. Existing path components
// are evaluated through symlinks; a symlink whose target escapes the root is
// rejected even when its lexical path looks safe.
func (r *Root) Resolve(name string) (string, error) {
	if r == nil || r.Dir == "" {
		return "", errors.New("fsguard: unopened root")
	}
	var candidate string
	if filepath.IsAbs(name) {
		candidate = filepath.Clean(name)
	} else {
		candidate = filepath.Join(r.Dir, name)
	}
	if !within(r.Dir, candidate) {
		return "", fmt.Errorf("%w: %s", ErrEscape, name)
	}
	// Walk existing components, resolving symlinks one at a time so that the
	// final (possibly not yet existing) component may legitimately not exist.
	if err := r.checkSymlinks(candidate); err != nil {
		return "", err
	}
	// If the file exists, re-check after full evaluation.
	if real, err := filepath.EvalSymlinks(candidate); err == nil {
		if !within(r.Dir, real) {
			return "", fmt.Errorf("%w: %s -> %s", ErrEscape, name, real)
		}
		return real, nil
	}
	return candidate, nil
}

// checkSymlinks resolves each existing component of p.
func (r *Root) checkSymlinks(p string) error {
	rel, err := filepath.Rel(r.Dir, p)
	if err != nil {
		return fmt.Errorf("%w: %s", ErrEscape, p)
	}
	if rel == "." {
		return nil
	}
	cur := r.Dir
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		if part == "" || part == "." {
			continue
		}
		cur = filepath.Join(cur, part)
		info, err := os.Lstat(cur)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if info.Mode()&os.ModeSymlink == 0 {
			continue
		}
		if !r.FollowSymlinks {
			return fmt.Errorf("%w: symlink %s", ErrEscape, cur)
		}
		target, err := filepath.EvalSymlinks(cur)
		if err != nil {
			return err
		}
		if !within(r.Dir, target) {
			return fmt.Errorf("%w: symlink %s -> %s", ErrEscape, cur, target)
		}
		cur = target
	}
	return nil
}

func within(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return false
	}
	return rel == "." || (!strings.HasPrefix(rel, ".."+string(filepath.Separator)) && rel != "..")
}

// Open opens a confined file for reading.
func (r *Root) Open(name string) (*os.File, string, error) {
	resolved, err := r.Resolve(name)
	if err != nil {
		return nil, "", err
	}
	f, err := os.Open(resolved)
	if err != nil {
		return nil, "", err
	}
	return f, resolved, nil
}

// AtomicFile creates a temporary output next to the target and renames it into
// place only after Commit. Close before Commit deletes the partial file, which
// keeps failed exports from being mistaken for finished ones.
type AtomicFile struct {
	Target string
	tmp    *os.File
	done   bool
}

// CreateOutput creates the temporary file in the target's directory.
func CreateOutput(target string) (*AtomicFile, error) {
	dir := filepath.Dir(target)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	f, err := os.CreateTemp(dir, "."+filepath.Base(target)+".tmp-*")
	if err != nil {
		return nil, err
	}
	return &AtomicFile{Target: target, tmp: f}, nil
}

// Write implements io.Writer.
func (a *AtomicFile) Write(p []byte) (int, error) { return a.tmp.Write(p) }

// Seek implements io.Seeker, which the MP4 writer needs to patch box sizes.
func (a *AtomicFile) Seek(offset int64, whence int) (int64, error) {
	return a.tmp.Seek(offset, whence)
}

// Sync flushes to stable storage.
func (a *AtomicFile) Sync() error { return a.tmp.Sync() }

// Commit renames the temporary file onto the target.
func (a *AtomicFile) Commit() error {
	if a.done {
		return errors.New("fsguard: output already committed")
	}
	if err := a.tmp.Close(); err != nil {
		_ = os.Remove(a.tmp.Name())
		return err
	}
	if err := os.Rename(a.tmp.Name(), a.Target); err != nil {
		_ = os.Remove(a.tmp.Name())
		return err
	}
	a.done = true
	return nil
}

// Abort removes the incomplete output. It is safe after Commit.
func (a *AtomicFile) Abort() error {
	if a == nil || a.done {
		return nil
	}
	a.done = true
	name := a.tmp.Name()
	_ = a.tmp.Close()
	return os.Remove(name)
}

// Close aborts unless Commit already succeeded.
func (a *AtomicFile) Close() error { return a.Abort() }

// Name returns the temporary path (useful in tests).
func (a *AtomicFile) Name() string {
	if a == nil || a.tmp == nil {
		return ""
	}
	return a.tmp.Name()
}
