// Package safepath confines all temporary and output artifacts to an explicit
// work directory and rejects traversal or symlinked escapes.
package safepath

import (
	"os"
	"path/filepath"
	"strings"

	"mediamerge/internal/model"
)

// Join resolves name underneath dir and fails when the result escapes dir.
// Absolute names, ".." segments, and names that clean outside the root are
// rejected before any filesystem operation.
func Join(dir, name string) (string, error) {
	if name == "" {
		return "", model.NewError(model.StageVerify, model.CodePath, -1, 0, false,
			"empty artifact name")
	}
	if filepath.IsAbs(name) {
		return "", model.NewError(model.StageVerify, model.CodePath, -1, 0, false,
			"absolute artifact path rejected: %s", name)
	}
	clean := filepath.Clean(name)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", model.NewError(model.StageVerify, model.CodePath, -1, 0, false,
			"path traversal rejected: %s", name)
	}
	root, err := filepath.Abs(dir)
	if err != nil {
		return "", model.WrapError(model.StageVerify, model.CodePath, -1, 0, false, err,
			"resolve work directory")
	}
	full := filepath.Join(root, clean)
	rel, err := filepath.Rel(root, full)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", model.NewError(model.StageVerify, model.CodePath, -1, 0, false,
			"artifact escapes work directory: %s", name)
	}
	return full, nil
}

// EnsureDir creates dir and rejects a symlinked work directory so artifacts
// cannot be redirected outside the approved location.
func EnsureDir(dir string) error {
	info, err := os.Lstat(dir)
	if err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return model.NewError(model.StageVerify, model.CodePath, 0, 0, false,
				"work directory is a symlink: %s", dir)
		}
		if !info.IsDir() {
			return model.NewError(model.StageVerify, model.CodePath, 0, 0, false,
				"work path is not a directory: %s", dir)
		}
		return nil
	}
	if os.IsNotExist(err) {
		return os.MkdirAll(dir, 0o755)
	}
	return model.WrapError(model.StageVerify, model.CodePath, 0, 0, false, err,
		"stat work directory")
}

// EnsureFile rejects a symlinked artifact.
func EnsureFile(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return model.WrapError(model.StageVerify, model.CodePath, 0, 0, false, err, "lstat artifact")
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return model.NewError(model.StageVerify, model.CodePath, 0, 0, false,
			"artifact is a symlink: %s", path)
	}
	return nil
}
