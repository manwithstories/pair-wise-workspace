// Package store provides a task-directory jail, a write-ahead log and
// same-volume atomic commits with checksums.
package store

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"

	"pairflow/internal/media"
)

// Jail restricts all artifact IO to one task directory. It refuses absolute
// escapes, ".." segments and symlinks in either the directory or the target.
type Jail struct {
	root string
}

// OpenJail resolves root and validates it is a real directory, not a symlink.
func OpenJail(root string) (*Jail, *media.Error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, media.Wrap(err, media.ClassInput, "store.root", "cannot resolve root").WithPath(root)
	}
	info, err := os.Lstat(abs)
	if err != nil {
		return nil, media.Wrap(err, media.ClassInput, "store.root_missing", "task directory does not exist").WithPath(root)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, media.Input("store.root_symlink", "task directory must not be a symlink").WithPath(root)
	}
	if !info.IsDir() {
		return nil, media.Input("store.root_not_dir", "task path is not a directory").WithPath(root)
	}
	return &Jail{root: abs}, nil
}

// Root is the canonical task directory.
func (j *Jail) Root() string { return j.root }

// Resolve maps a relative path inside the jail to an absolute path, rejecting
// escapes and existing symlinks.
func (j *Jail) Resolve(rel string) (string, *media.Error) {
	if rel == "" {
		return "", media.Input("store.empty_path", "empty artifact path")
	}
	if filepath.IsAbs(rel) {
		return "", media.Input("store.absolute_path", "artifact paths must be relative to the task directory").WithPath(rel)
	}
	clean := filepath.Clean(rel)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", media.Input("store.escape", "artifact path escapes the task directory").WithPath(rel)
	}
	if strings.Contains(rel, string(filepath.Separator)+".."+string(filepath.Separator)) {
		return "", media.Input("store.escape", "artifact path contains a parent segment").WithPath(rel)
	}
	abs := filepath.Join(j.root, clean)
	// Refuse symlinked components, walking from the root down.
	cur := j.root
	for _, seg := range strings.Split(clean, string(filepath.Separator)) {
		cur = filepath.Join(cur, seg)
		info, err := os.Lstat(cur)
		if err != nil {
			if os.IsNotExist(err) {
				break // remaining components do not exist yet
			}
			return "", media.Wrap(err, media.ClassInput, "store.stat", "cannot inspect artifact path").WithPath(rel)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", media.Input("store.symlink", "artifact path traverses a symlink").WithPath(rel)
		}
	}
	return abs, nil
}

// EnsureDir resolves a relative directory and creates it and its parents.
func (j *Jail) EnsureDir(rel string) (string, *media.Error) {
	abs, err := j.Resolve(rel)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return "", media.Wrap(err, media.ClassInternal, "store.mkdir", "cannot create directory").WithPath(rel)
	}
	return abs, nil
}

// ReadFile reads a jailed file.
func (j *Jail) ReadFile(rel string) ([]byte, *media.Error) {
	abs, err := j.Resolve(rel)
	if err != nil {
		return nil, err
	}
	data, oerr := os.ReadFile(abs)
	if oerr != nil {
		return nil, media.Wrap(oerr, media.ClassInput, "store.read", "cannot read artifact").WithPath(rel)
	}
	return data, nil
}

// HashFile returns the SHA-256 of a jailed file and its size. It streams in
// bounded blocks so large frame sequences do not need to fit in memory.
func (j *Jail) HashFile(rel string) (hash string, size int64, err *media.Error) {
	abs, rerr := j.Resolve(rel)
	if rerr != nil {
		return "", 0, rerr
	}
	f, oerr := os.Open(abs)
	if oerr != nil {
		return "", 0, media.Wrap(oerr, media.ClassInput, "store.open", "cannot open artifact").WithPath(rel)
	}
	defer f.Close()
	h := sha256.New()
	buf := make([]byte, 64*1024)
	n, cerr := io.CopyBuffer(h, f, buf)
	if cerr != nil {
		return "", 0, media.Wrap(cerr, media.ClassInternal, "store.hash", "cannot hash artifact").WithPath(rel)
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// HashBytes returns the SHA-256 of an in-memory artifact.
func HashBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// SyncFile fsyncs a jailed file, returning its size.
func (j *Jail) SyncFile(rel string) (int64, *media.Error) {
	abs, err := j.Resolve(rel)
	if err != nil {
		return 0, err
	}
	f, oerr := os.Open(abs)
	if oerr != nil {
		return 0, media.Wrap(oerr, media.ClassInput, "store.sync_open", "cannot open artifact for sync").WithPath(rel)
	}
	defer f.Close()
	if serr := f.Sync(); serr != nil {
		return 0, media.Wrap(serr, media.ClassInternal, "store.sync", "cannot fsync artifact").WithPath(rel)
	}
	info, ierr := f.Stat()
	if ierr != nil {
		return 0, media.Wrap(ierr, media.ClassInternal, "store.sync_stat", "cannot stat artifact").WithPath(rel)
	}
	return info.Size(), nil
}
