package core

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// ErrPathEscape is returned when a manifest reference resolves outside of the
// root directory that the caller explicitly authorized.
var ErrPathEscape = errors.New("core: manifest path escapes input root")

// SafeJoin resolves ref against root and guarantees the result stays inside
// root. Absolute refs, refs containing ".." traversal or symlinks that point
// outside root are rejected. Symlinks inside root that stay within root are
// allowed. All checks operate on cleaned absolute paths.
func SafeJoin(root, ref string) (string, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	if filepath.IsAbs(ref) {
		return "", ErrPathEscape
	}
	clean := filepath.Clean(filepath.Join(absRoot, filepath.FromSlash(ref)))
	if !within(clean, absRoot) {
		return "", ErrPathEscape
	}
	realRoot, err := filepath.EvalSymlinks(absRoot)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		realRoot = absRoot
	}
	realTarget, err := evalParentSymlinks(clean)
	if err != nil {
		return "", err
	}
	if !within(realTarget, realRoot) {
		return "", ErrPathEscape
	}
	return clean, nil
}

func within(path, root string) bool {
	if path == root {
		return true
	}
	return strings.HasPrefix(path, root+string(filepath.Separator))
}

// evalParentSymlinks resolves symlinks on every existing parent of target,
// tolerating the final component not existing yet (outputs may be created).
func evalParentSymlinks(target string) (string, error) {
	parent := filepath.Dir(target)
	realParent, err := filepath.EvalSymlinks(parent)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return target, nil
		}
		return "", err
	}
	return filepath.Join(realParent, filepath.Base(target)), nil
}
