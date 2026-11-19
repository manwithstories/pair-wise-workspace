// Package safepath confines every file the tool touches to the work
// directory and rejects symlink escapes.
package safepath

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Join resolves rel against root and verifies the result stays inside
// root, even when path components are symlinks. Works for paths that do
// not exist yet (the nearest existing ancestor is evaluated).
func Join(root, rel string) (string, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	evalRoot, err := filepath.EvalSymlinks(absRoot)
	if err != nil {
		return "", fmt.Errorf("work dir: %w", err)
	}
	target := filepath.Join(evalRoot, rel)
	// Walk up to the nearest existing ancestor and evaluate it.
	p := target
	var tail []string
	for {
		if _, err := os.Lstat(p); err == nil {
			break
		}
		tail = append([]string{filepath.Base(p)}, tail...)
		p = filepath.Dir(p)
		if p == filepath.Dir(p) {
			return "", fmt.Errorf("no existing ancestor for %q", rel)
		}
	}
	eval, err := filepath.EvalSymlinks(p)
	if err != nil {
		return "", fmt.Errorf("resolve %q: %w", rel, err)
	}
	resolved := filepath.Join(append([]string{eval}, tail...)...)
	if resolved != evalRoot && !strings.HasPrefix(resolved, evalRoot+string(os.PathSeparator)) {
		return "", fmt.Errorf("path %q escapes work directory", rel)
	}
	return resolved, nil
}
