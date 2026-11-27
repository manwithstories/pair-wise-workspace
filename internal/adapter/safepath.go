package adapter

import (
	"os"
	"path/filepath"
	"strings"
)

// ValidateInput applies the local-only input policy: no network/pipe sources,
// no path traversal, no symlinks and no non-regular files. It inspects the
// path itself rather than resolving symlinks, so a symlinked input is always
// rejected before any tool ever opens it.
func ValidateInput(path string) error {
	if path == "" {
		return NewError(CodeBadArgument, "empty input path", Source{})
	}
	if strings.ContainsRune(path, 0) {
		return NewError(CodePathRejected, "NUL byte in path", Source{File: path})
	}
	// No remote or pseudo inputs: avcore is strictly local/offline.
	if strings.Contains(path, "://") || strings.HasPrefix(path, "pipe:") {
		return NewError(CodePathRejected, "remote/pipe inputs are forbidden", Source{File: path})
	}
	// Reject traversal on the raw components: filepath.Clean would resolve
	// ".." before we could see it, so inspect the un-cleaned separators.
	for _, seg := range strings.FieldsFunc(path, func(r rune) bool {
		return r == filepath.Separator || r == '/'
	}) {
		if seg == ".." {
			return NewError(CodePathRejected, "path traversal segment rejected", Source{File: path})
		}
	}
	clean := filepath.Clean(path)
	info, err := os.Lstat(clean)
	if err != nil {
		return NewError(CodePathRejected, "cannot stat input: "+err.Error(), Source{File: path})
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return NewError(CodeSymlink, "symlinked inputs are forbidden", Source{File: path})
	}
	if !info.Mode().IsRegular() {
		return NewError(CodePathRejected, "input is not a regular file", Source{File: path})
	}
	return nil
}

// Confine ensures path, after cleaning, stays inside root. It is used for all
// generated outputs so a crafted segment name can never escape the job dir.
func Confine(root, path string) (string, error) {
	absRoot, err := filepath.Abs(filepath.Clean(root))
	if err != nil {
		return "", NewError(CodeInternal, "bad root: "+err.Error(), Source{})
	}
	abs, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return "", NewError(CodeBadArgument, "bad path: "+err.Error(), Source{File: path})
	}
	if abs != absRoot && !strings.HasPrefix(abs, absRoot+string(filepath.Separator)) {
		return "", NewError(CodePathRejected, "path escapes job directory", Source{File: path})
	}
	return abs, nil
}
