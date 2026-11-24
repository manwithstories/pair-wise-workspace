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
// allowed. All checks operate on cleaned, symlink-resolved absolute paths.
func SafeJoin(root, ref string) (string, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	if filepath.IsAbs(ref) {
		return "", ErrPathEscape
	}
	// Resolve the root first: on macOS /var is a symlink to /private/var,
	// otherwise joined paths would carry the un-resolved prefix and every
	// containment check would fail.
	realRoot, err := filepath.EvalSymlinks(absRoot)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			realRoot = absRoot
		} else {
			return "", err
		}
	}
	clean := filepath.Clean(filepath.Join(realRoot, filepath.FromSlash(ref)))
	if !within(clean, realRoot) {
		return "", ErrPathEscape
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

// evalParentSymlinks walks target from the root downward, resolving every
// existing symlink component (including the final component) while tolerating
// trailing components that do not exist yet — outputs may be created later.
func evalParentSymlinks(target string) (string, error) {
	parts := strings.Split(filepath.Clean(target), string(filepath.Separator))
	cur := string(filepath.Separator)
	for i, part := range parts {
		if part == "" {
			continue
		}
		next := filepath.Join(cur, part)
		real, err := filepath.EvalSymlinks(next)
		switch {
		case err == nil:
			cur = real
		case errors.Is(err, os.ErrNotExist):
			// This component does not exist; neither can anything below it.
			rest := append([]string{cur}, parts[i:]...)
			return filepath.Join(rest...), nil
		default:
			return "", err
		}
	}
	return cur, nil
}
