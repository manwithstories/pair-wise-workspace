// Package safeio confines every file access to an explicit root directory and
// rejects symbolic links that escape it. Media recovery runs against untrusted
// archive payloads, so path traversal and symlink escape are treated as hard
// errors rather than conveniences to normalise away.
package safeio

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ErrEscape is returned when a path would leave the configured root.
var ErrEscape = errors.New("safeio: path escapes root")

// ErrSymlink is returned when a symlink resolves outside the root.
var ErrSymlink = errors.New("safeio: symlink escapes root")

// ErrAbsolute is returned when a relative path is required but absent.
var ErrAbsolute = errors.New("safeio: absolute path not allowed")

// Resolve joins rel onto root and guarantees the result stays inside root.
// It rejects absolute inputs, ".." traversal, and symlinks (in any component)
// whose target leaves the root. The returned path is clean and absolute.
func Resolve(root, rel string) (string, error) {
	if root == "" {
		return "", fmt.Errorf("%w: empty root", ErrEscape)
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	// The root itself may traverse symlinks (macOS /var -> /private/var), so
	// compare against its resolved form.
	if resolvedRoot, err := filepath.EvalSymlinks(absRoot); err == nil {
		absRoot = resolvedRoot
	}
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("%w: %s", ErrAbsolute, rel)
	}
	cleaned := filepath.Clean(rel)
	if cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: %s", ErrEscape, rel)
	}
	joined := filepath.Join(absRoot, cleaned)

	// Lexical containment check first (cheap, catches most attacks).
	if !within(absRoot, joined) {
		return "", fmt.Errorf("%w: %s", ErrEscape, rel)
	}

	// Symlink check: resolve the longest existing prefix and confirm it stays
	// inside root. Missing tail components are fine (they cannot be symlinks).
	resolved, err := resolveExisting(joined)
	if err != nil {
		return "", err
	}
	if !within(absRoot, resolved) {
		return "", fmt.Errorf("%w: %s", ErrSymlink, rel)
	}
	return resolved, nil
}

// within reports whether child is root or inside it.
func within(root, child string) bool {
	if root == child {
		return true
	}
	rel, err := filepath.Rel(root, child)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// resolveExisting returns the path with symlinks resolved as far as the file
// system allows. If the path does not exist at all it returns the cleaned
// absolute path; its parent chain is still checked by the caller via
// EvalSymlinks on the deepest existing ancestor.
func resolveExisting(p string) (string, error) {
	cur := p
	var tail []string
	for {
		if resolved, err := filepath.EvalSymlinks(cur); err == nil {
			for i := len(tail) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, tail[i])
			}
			return resolved, nil
		} else if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return p, nil
		}
		tail = append(tail, filepath.Base(cur))
		cur = parent
	}
}
