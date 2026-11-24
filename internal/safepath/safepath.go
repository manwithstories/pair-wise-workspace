// Package safepath confines all file access to an explicit work directory:
// every joined path is cleaned, made absolute and verified to stay inside the
// root, so no input or staging file can escape the local workspace.
package safepath

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Root is an absolute directory that all access must remain inside.
type Root struct {
	abs string
}

// NewRoot resolves dir to an absolute path.
func NewRoot(dir string) (*Root, error) {
	abs, err := filepath.Abs(filepath.Clean(dir))
	if err != nil {
		return nil, err
	}
	return &Root{abs: abs}, nil
}

// String returns the absolute root path.
func (r *Root) String() string { return r.abs }

// Join joins elems under the root and rejects any result that escapes it.
func (r *Root) Join(elems ...string) (string, error) {
	target := filepath.Join(append([]string{r.abs}, elems...)...)
	target = filepath.Clean(target)
	if target != r.abs && !strings.HasPrefix(target, r.abs+string(os.PathSeparator)) {
		return "", fmt.Errorf("safepath: %q escapes work directory %q", target, r.abs)
	}
	return target, nil
}

// MustJoin panics on violation; only use with hard-coded relative fragments.
func (r *Root) MustJoin(elems ...string) string {
	p, err := r.Join(elems...)
	if err != nil {
		panic(err)
	}
	return p
}

// Contains reports whether target is the root or inside it.
func (r *Root) Contains(target string) bool {
	abs, err := filepath.Abs(filepath.Clean(target))
	if err != nil {
		return false
	}
	return abs == r.abs || strings.HasPrefix(abs, r.abs+string(os.PathSeparator))
}
