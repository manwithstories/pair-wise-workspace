// Package safe confines all filesystem access to a caller-selected root.
package safe

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"bcast/internal/fault"
)

// Root is an absolute, symlink-resolved write/read boundary.
type Root struct {
	path string
}

// OpenRoot resolves and validates the root directory.
func OpenRoot(path string) (*Root, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fault.E(fault.PathRejected, "safe", "OpenRoot", err).WithPath(path)
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, fault.E(fault.PathRejected, "safe", "OpenRoot", err).WithPath(abs)
	}
	st, err := os.Stat(real)
	if err != nil {
		return nil, fault.E(fault.PathRejected, "safe", "OpenRoot", err).WithPath(real)
	}
	if !st.IsDir() {
		return nil, fault.E(fault.PathRejected, "safe", "OpenRoot", errors.New("root is not a directory")).WithPath(real)
	}
	return &Root{path: real}, nil
}

// Path returns the absolute resolved root.
func (r *Root) Path() string { return r.path }

// Resolve joins rel to root, rejects traversal and rejects symlinks among
// existing path components. Missing output components are allowed.
func (r *Root) Resolve(rel string) (string, error) {
	if rel == "" {
		return r.path, nil
	}
	if filepath.IsAbs(rel) || strings.ContainsRune(rel, 0) {
		return "", fault.E(fault.PathRejected, "safe", "Resolve", errors.New("absolute or NUL path rejected")).WithPath(rel)
	}
	clean := filepath.Clean(rel)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fault.E(fault.PathRejected, "safe", "Resolve", errors.New("path traversal rejected")).WithPath(rel)
	}
	full := filepath.Join(r.path, clean)
	relToRoot, err := filepath.Rel(r.path, full)
	if err != nil || relToRoot == ".." || strings.HasPrefix(relToRoot, ".."+string(filepath.Separator)) {
		return "", fault.E(fault.PathRejected, "safe", "Resolve", errors.New("path escapes root")).WithPath(rel)
	}

	// Walk existing components from the root and reject every symlink.
	cur := r.path
	for _, part := range strings.Split(filepath.ToSlash(clean), "/") {
		if part == "." || part == "" {
			continue
		}
		cur = filepath.Join(cur, part)
		li, lerr := os.Lstat(cur)
		if lerr != nil {
			if errors.Is(lerr, fs.ErrNotExist) {
				break
			}
			return "", fault.E(fault.PathRejected, "safe", "Resolve", lerr).WithPath(cur)
		}
		if li.Mode()&os.ModeSymlink != 0 {
			return "", fault.E(fault.PathRejected, "safe", "Resolve", errors.New("symlink rejected")).WithPath(cur)
		}
	}

	// For an existing target, fully resolve links and re-check containment.
	if real, err := filepath.EvalSymlinks(full); err == nil {
		relReal, rerr := filepath.Rel(r.path, real)
		if rerr != nil || relReal == ".." || strings.HasPrefix(relReal, ".."+string(filepath.Separator)) {
			return "", fault.E(fault.PathRejected, "safe", "Resolve", errors.New("resolved path escapes root")).WithPath(real)
		}
		return real, nil
	}
	return full, nil
}

// MkdirAll creates a confined directory.
func (r *Root) MkdirAll(rel string) (string, error) {
	p, err := r.Resolve(rel)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(p, 0o755); err != nil {
		return "", fault.E(fault.WriteFailed, "safe", "MkdirAll", err).WithPath(p)
	}
	return p, nil
}

// ReadFile reads a confined file.
func (r *Root) ReadFile(rel string) ([]byte, error) {
	p, err := r.Resolve(rel)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, fault.E(fault.Internal, "safe", "ReadFile", err).WithPath(p)
	}
	return b, nil
}

// WriteFile writes a confined file after creating its parent.
func (r *Root) WriteFile(rel string, data []byte, mode os.FileMode) error {
	p, err := r.Resolve(rel)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return fault.E(fault.WriteFailed, "safe", "WriteFile", err).WithPath(p)
	}
	if err := os.WriteFile(p, data, mode); err != nil {
		return fault.E(fault.WriteFailed, "safe", "WriteFile", err).WithPath(p)
	}
	return nil
}
