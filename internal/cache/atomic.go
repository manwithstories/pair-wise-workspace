package cache

import (
	"io"
	"os"
	"path/filepath"

	"github.com/fashionweek/mediad/internal/media"
)

// writeFileAtomic writes data through a temp file and renames into place so a
// reader never observes a partially written target.
func writeFileAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return media.New(media.PhaseCache, media.KindTool, path, -1,
			"cannot create target directory").Wrap(err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return media.New(media.PhaseCache, media.KindTool, path, -1,
			"cannot create temp file").Wrap(err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return media.New(media.PhaseCache, media.KindTool, path, -1,
			"temp write failed").Wrap(err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return media.New(media.PhaseCache, media.KindTool, path, -1,
			"temp fsync failed").Wrap(err)
	}
	tmp.Close()
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return media.New(media.PhaseCache, media.KindTool, path, -1,
			"atomic rename failed").Wrap(err)
	}
	return nil
}

// MoveAtomic publishes src to dst via rename, falling back to a streamed copy
// when the two paths live on different filesystems.
func MoveAtomic(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return media.New(media.PhaseCache, media.KindTool, dst, -1,
			"cannot create destination directory").Wrap(err)
	}
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	return copyAtomic(src, dst)
}

// copyAtomic streams src into a temp file at dst and atomically renames.
func copyAtomic(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return media.New(media.PhaseCache, media.KindTool, src, -1,
			"cannot open source for copy").Wrap(err)
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return media.New(media.PhaseCache, media.KindTool, dst, -1,
			"cannot create destination directory").Wrap(err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".tmp-*")
	if err != nil {
		return media.New(media.PhaseCache, media.KindTool, dst, -1,
			"cannot create temp destination").Wrap(err)
	}
	tmpName := tmp.Name()
	if _, err := io.Copy(tmp, in); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return media.New(media.PhaseCache, media.KindTool, dst, -1,
			"stream copy failed").Wrap(err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return media.New(media.PhaseCache, media.KindTool, dst, -1,
			"destination fsync failed").Wrap(err)
	}
	tmp.Close()
	if err := os.Rename(tmpName, dst); err != nil {
		os.Remove(tmpName)
		return media.New(media.PhaseCache, media.KindTool, dst, -1,
			"atomic rename failed").Wrap(err)
	}
	return nil
}
