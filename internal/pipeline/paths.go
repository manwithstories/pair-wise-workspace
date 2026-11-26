package pipeline

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/pairwise/relay/internal/media"
)

func safeJoin(root string, parts ...string) (string, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", media.NewExitError(4, err)
	}
	joined := filepath.Join(append([]string{absRoot}, parts...)...)
	abs, err := filepath.Abs(joined)
	if err != nil {
		return "", media.NewExitError(4, err)
	}
	rel, err := filepath.Rel(absRoot, abs)
	if err != nil {
		return "", media.NewExitError(4, err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", media.NewFault(media.CodeUnsafePath, media.FragmentID{}, -1, false, "path escapes work directory: %v", parts)
	}
	return abs, nil
}

func writeAtomic(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
