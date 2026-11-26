package pipeline

import (
	"path/filepath"
	"strings"

	"mediad/internal/model"
)

// SafeJoin joins name under root and rejects absolute names and any result
// that escapes root through parent traversal.
func SafeJoin(root, name string) (string, error) {
	if name == "" {
		return "", model.New(model.CodeInvalidArgument, model.StageCLI, "empty file name")
	}
	if filepath.IsAbs(name) {
		return "", model.New(model.CodePathEscape, model.StageCLI, "absolute artifact path is not allowed").WithSource(name)
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", model.Wrap(model.CodeInvalidArgument, model.StageCLI, "resolve work root", err)
	}
	cleanName := filepath.Clean(name)
	if cleanName == ".." || strings.HasPrefix(cleanName, ".."+string(filepath.Separator)) {
		return "", model.New(model.CodePathEscape, model.StageCLI, "artifact name escapes work directory").WithSource(name)
	}
	full := filepath.Join(absRoot, cleanName)
	rel, err := filepath.Rel(absRoot, full)
	if err != nil {
		return "", model.Wrap(model.CodePathEscape, model.StageCLI, "check artifact path", err).WithSource(name)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", model.New(model.CodePathEscape, model.StageCLI, "artifact path escapes work directory").WithSource(name)
	}
	return full, nil
}
