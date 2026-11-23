// Package safeio confines all filesystem access to a workspace root and
// rejects symlink escapes.
package safeio

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/example/mediad/internal/model"
)

// Resolve makes path absolute, cleans it, and guarantees it stays within
// root. Symlinked components are rejected outright.
func Resolve(root, path string) (string, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", model.Wrap(model.KindArgument, "safeio", "resolve", path, err)
	}
	absRoot = filepath.Clean(absRoot)

	abs := path
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(absRoot, abs)
	}
	abs = filepath.Clean(abs)
	if abs != absRoot && !strings.HasPrefix(abs, absRoot+string(os.PathSeparator)) {
		return "", model.New(model.KindArgument, "safeio", "resolve",
			"path escapes workspace root: "+path)
	}

	// Walk every component and reject symlinks, including missing leaves
	// whose parent directory is itself a symlink.
	rel, err := filepath.Rel(absRoot, abs)
	if err != nil {
		return "", model.Wrap(model.KindArgument, "safeio", "resolve", path, err)
	}
	cur := absRoot
	for _, part := range strings.Split(rel, string(os.PathSeparator)) {
		if part == "." || part == "" {
			continue
		}
		cur = filepath.Join(cur, part)
		fi, err := os.Lstat(cur)
		if err != nil {
			if os.IsNotExist(err) {
				continue // output path not created yet
			}
			return "", model.Wrap(model.KindArgument, "safeio", "resolve", cur, err)
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return "", model.New(model.KindArgument, "safeio", "resolve",
				"symlink rejected: "+cur)
		}
	}
	return abs, nil
}

// OpenInput resolves and opens an existing file read-only.
func OpenInput(root, path string) (*os.File, string, error) {
	abs, err := Resolve(root, path)
	if err != nil {
		return nil, "", err
	}
	f, err := os.Open(abs)
	if err != nil {
		return nil, "", model.Wrap(model.KindCorrupt, "safeio", "open", abs, err)
	}
	return f, abs, nil
}
