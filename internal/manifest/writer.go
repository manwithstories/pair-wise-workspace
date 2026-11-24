package manifest

import (
	"bufio"
	"crypto/sha256"
	"encoding/csv"
	"fmt"
	"hash"
	"os"
	"path/filepath"
	"strconv"

	"mediad/internal/index"
)

// AtomicWriter streams manifest rows to a temp file and commits by rename. The
// SHA-256 is accumulated over every byte written, so the returned digest is of
// the exact file contents without a second read.
type AtomicWriter struct {
	f      *os.File
	name   string
	dest   string
	bw     *bufio.Writer
	cw     *csv.Writer
	h      hash.Hash
	rows   int
	row    []string
	closed bool
}

// NewAtomicWriter creates the temp file and writes the header row.
func NewAtomicWriter(dest string) (*AtomicWriter, error) {
	dir := filepath.Dir(dest)
	f, err := os.CreateTemp(dir, ".manifest-*.tmp")
	if err != nil {
		return nil, err
	}
	w := &AtomicWriter{f: f, name: f.Name(), dest: dest, h: sha256.New(), row: make([]string, len(Header))}
	w.bw = bufio.NewWriter(ioTee(w.h, f))
	w.cw = csv.NewWriter(w.bw)
	if err := w.cw.Write(Header); err != nil {
		return nil, err
	}
	return w, nil
}

// WriteRow appends one entry as a manifest row.
func (w *AtomicWriter) WriteRow(e index.Entry) error {
	w.row[0] = strconv.FormatUint(uint64(e.TrackID), 10)
	w.row[1] = strconv.FormatUint(e.FrameNo, 10)
	w.row[2] = strconv.FormatInt(e.PTS, 10)
	w.row[3] = strconv.FormatInt(e.Offset, 10)
	w.row[4] = strconv.FormatUint(uint64(e.Size), 10)
	w.row[5] = fmt.Sprintf("%x", e.Digest)
	w.row[6] = "ok"
	if err := w.cw.Write(w.row); err != nil {
		return err
	}
	w.rows++
	return nil
}

// Sync flushes buffered CSV bytes and the underlying file.
func (w *AtomicWriter) Sync() error {
	w.cw.Flush()
	if err := w.cw.Error(); err != nil {
		return err
	}
	if err := w.bw.Flush(); err != nil {
		return err
	}
	return w.f.Sync()
}

// Finish flushes all buffered output. The file is not yet renamed.
func (w *AtomicWriter) Finish() error { return w.Sync() }

// Digest returns the digest of the flushed bytes. It must be called after
// Finish (or Sync), since buffered bytes have not been hashed yet.
func (w *AtomicWriter) Digest() [32]byte {
	var d [32]byte
	copy(d[:], w.h.Sum(nil))
	return d
}

// Rows returns the number of data rows written.
func (w *AtomicWriter) Rows() int { return w.rows }

// Abort removes the temp file without publishing it.
func (w *AtomicWriter) Abort() {
	if w.closed {
		return
	}
	w.closed = true
	w.f.Close()
	os.Remove(w.name)
}

// Commit fsyncs and atomically renames the temp file over the destination.
func (w *AtomicWriter) Commit() error {
	if w.closed {
		return fmt.Errorf("manifest: writer already closed")
	}
	if err := w.Sync(); err != nil {
		return err
	}
	if err := w.f.Close(); err != nil {
		return err
	}
	w.closed = true
	return os.Rename(w.name, w.dest)
}

// ioTee is a tiny io.Writer fan-out that avoids importing io just for MultiWriter
// semantics here while keeping the hash perfectly in step with the file.
func ioTee(h hash.Hash, f *os.File) *teeWriter { return &teeWriter{h: h, f: f} }

type teeWriter struct {
	h hash.Hash
	f *os.File
}

func (t *teeWriter) Write(p []byte) (int, error) {
	n, err := t.f.Write(p)
	if n > 0 {
		t.h.Write(p[:n])
	}
	return n, err
}
