package archive

import (
	"io"
	"os"
	"path/filepath"

	"archivecheck/internal/media"
)

// writeAtomic writes data to path via a sibling temp file, fsync and rename, so
// readers never observe a partially written target.
func writeAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return media.NewFault(media.KindIO, filepath.Base(path), 0, -1, "mkdir: %s", err)
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return media.NewFault(media.KindIO, filepath.Base(path), 0, -1, "create: %s", err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return media.NewFault(media.KindIO, filepath.Base(path), 0, -1, "write: %s", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return media.NewFault(media.KindIO, filepath.Base(path), 0, -1, "fsync: %s", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return media.NewFault(media.KindIO, filepath.Base(path), 0, -1, "close: %s", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return media.NewFault(media.KindIO, filepath.Base(path), 0, -1, "rename: %s", err)
	}
	return nil
}

// copyAtomic streams src to a temp file at dst and renames into place.
func copyAtomic(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return media.NewFault(media.KindIO, filepath.Base(src), 0, -1, "open src: %s", err)
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return media.NewFault(media.KindIO, filepath.Base(dst), 0, -1, "mkdir: %s", err)
	}
	tmp := dst + ".tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return media.NewFault(media.KindIO, filepath.Base(dst), 0, -1, "create: %s", err)
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		_ = os.Remove(tmp)
		return media.NewFault(media.KindIO, filepath.Base(dst), 0, -1, "copy: %s", err)
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		_ = os.Remove(tmp)
		return media.NewFault(media.KindIO, filepath.Base(dst), 0, -1, "fsync: %s", err)
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(tmp)
		return media.NewFault(media.KindIO, filepath.Base(dst), 0, -1, "close: %s", err)
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return media.NewFault(media.KindIO, filepath.Base(dst), 0, -1, "rename: %s", err)
	}
	return nil
}

// copyLimited copies at most n bytes from r to w. n<=0 means unlimited.
func copyLimited(w io.Writer, r io.Reader, n int64) (int64, error) {
	if n <= 0 {
		return io.Copy(w, r)
	}
	return io.Copy(w, io.LimitReader(r, n))
}
