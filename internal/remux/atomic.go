// Package remux performs lossless container-to-container remuxing of mapped
// sample streams. Samples are copied byte-for-byte (no decode/re-encode),
// output goes to a temp file plus an atomic rename, and the writer is
// deterministic so identical input+config yields an identical output hash.
package remux

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"mediarun/internal/mediaerr"
)

// AtomicWriter stages output at <dest>.tmp and renames into place only on
// Commit. A crash or error leaves the temp file (cleaned up) and never a
// half-written destination. macOS APFS/HFS+ rename within one directory is
// atomic, which is all this type relies on.
type AtomicWriter struct {
	dest      string
	tmp       *os.File
	committed bool
}

// NewAtomicWriter opens the staging file inside the destination directory.
func NewAtomicWriter(dest string) (*AtomicWriter, error) {
	if filepath.Dir(dest) != "." {
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return nil, mediaerr.New(mediaerr.CodeIO, err.Error()).OnPath(dest)
		}
	}
	tmp, err := os.CreateTemp(filepath.Dir(dest), "."+filepath.Base(dest)+".tmp-*")
	if err != nil {
		return nil, mediaerr.New(mediaerr.CodeIO, err.Error()).OnPath(dest)
	}
	return &AtomicWriter{dest: dest, tmp: tmp}, nil
}

// Write feeds bytes to the temp file.
func (a *AtomicWriter) Write(p []byte) (int, error) {
	n, err := a.tmp.Write(p)
	if err != nil {
		return n, mediaerr.New(mediaerr.CodeIO, err.Error()).OnPath(a.dest)
	}
	return n, nil
}

// Path returns the staging path (used while the file is still uncommitted).
func (a *AtomicWriter) Path() string { return a.tmp.Name() }

// Size reports bytes written so far.
func (a *AtomicWriter) Size() int64 {
	st, err := a.tmp.Stat()
	if err != nil {
		return -1
	}
	return st.Size()
}

// Commit flushes, stamps are handled by callers via WriteAt, fsyncs, closes
// and atomically renames the temp file.
func (a *AtomicWriter) Commit() (string, error) {
	if a.committed {
		return "", mediaerr.New(mediaerr.CodeIO, "writer already committed").OnPath(a.dest)
	}
	if err := a.tmp.Sync(); err != nil {
		return "", a.abort(err)
	}
	if err := a.tmp.Close(); err != nil {
		return "", mediaerr.New(mediaerr.CodeIO, err.Error()).OnPath(a.dest)
	}
	if err := os.Rename(a.tmp.Name(), a.dest); err != nil {
		return "", mediaerr.New(mediaerr.CodeIO, err.Error()).OnPath(a.dest)
	}
	a.committed = true
	return a.dest, nil
}

// Abort closes and removes the staging file without touching the destination.
func (a *AtomicWriter) Abort() {
	if a.committed {
		return
	}
	_ = a.tmp.Close()
	_ = os.Remove(a.tmp.Name())
}

// HashFile returns the SHA-256 of the fully written (and possibly WriteAt-
// patched) temp file. It must be called before Commit because the temp path
// is renamed away during commit.
func (a *AtomicWriter) HashFile() (string, error) {
	if a.committed {
		return "", mediaerr.New(mediaerr.CodeIO, "cannot hash after commit").
			OnPath(a.dest)
	}
	if err := a.tmp.Sync(); err != nil {
		return "", mediaerr.New(mediaerr.CodeIO, err.Error()).OnPath(a.dest)
	}
	f, err := os.Open(a.tmp.Name())
	if err != nil {
		return "", mediaerr.New(mediaerr.CodeIO, err.Error()).OnPath(a.dest)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", mediaerr.New(mediaerr.CodeIO, err.Error()).OnPath(a.dest)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// WriteAt overwrites bytes at an absolute offset (used to stamp the frame
// count in place) without disturbing sequential payload writes.
func (a *AtomicWriter) WriteAt(p []byte, off int64) error {
	if _, err := a.tmp.WriteAt(p, off); err != nil {
		return mediaerr.New(mediaerr.CodeIO, err.Error()).OnPath(a.dest)
	}
	return nil
}

// abort closes/removes the temp file and wraps the cause.
func (a *AtomicWriter) abort(cause error) error {
	a.Abort()
	return mediaerr.New(mediaerr.CodeIO, cause.Error()).OnPath(a.dest)
}

// copyAll streams a reader fully into the writer, bounded by declared size,
// returning the byte count. It never buffers a whole sample in memory.
func copyAll(w io.Writer, r io.Reader, size int64) (int64, error) {
	if size < 0 {
		return 0, mediaerr.New(mediaerr.CodeNegativeDuration,
			fmt.Sprintf("negative payload size %d", size))
	}
	n, err := io.CopyN(w, r, size)
	if err != nil {
		return n, mediaerr.New(mediaerr.CodeIO, err.Error())
	}
	return n, nil
}
