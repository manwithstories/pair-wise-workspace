package remux

import (
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
)

// Commit finalises the container, syncs it to disk, verifies the size and
// atomically renames it over the destination.  It returns the SHA-256 of the
// delivered file.
func (w *Writer) Commit() (string, error) {
	if w.closed {
		return "", fmt.Errorf("remux: writer is closed")
	}
	if w.committed {
		return "", fmt.Errorf("remux: already committed")
	}
	if w.failed {
		// A run that saw a write failure or cancellation must never publish
		// whatever partial container reached the temporary file.
		return "", fmt.Errorf("remux: run was aborted before commit: %w", ErrAborted)
	}
	if err := w.ofc.WriteTrailer(); err != nil {
		return "", fmt.Errorf("remux: writing trailer: %w", err)
	}
	w.started = false
	// Flush the IO context before the descriptor is closed so no buffered
	// cluster is lost.  A custom IO context is released with Free, not
	// Close: Close is only valid for contexts opened by avio_open2.
	w.io.Flush()
	w.io.Free()
	w.io = nil
	if w.file != nil {
		if err := w.file.Sync(); err != nil {
			return "", fmt.Errorf("remux: syncing temporary file: %w", err)
		}
		if err := w.file.Close(); err != nil {
			return "", fmt.Errorf("remux: closing temporary file: %w", err)
		}
		w.file = nil
	}

	src, err := os.Open(w.temp)
	if err != nil {
		return "", fmt.Errorf("remux: reopening temporary file: %w", err)
	}
	sum, size, err := hashFile(src, w.hasher)
	src.Close()
	if err != nil {
		return "", err
	}
	if size == 0 {
		return "", fmt.Errorf("remux: temporary file is empty")
	}
	if err := os.Rename(w.temp, w.dest); err != nil {
		return "", fmt.Errorf("remux: committing %s: %w", w.dest, err)
	}
	// Persist the directory entry so the rename survives a crash.
	if d, derr := os.Open(filepath.Dir(w.dest)); derr == nil {
		_ = d.Sync()
		d.Close()
	}
	w.committed = true
	w.relinquish()
	return hex.EncodeToString(sum), nil
}

func hashFile(f *os.File, h hash.Hash) ([]byte, int64, error) {
	h.Reset()
	n, err := io.Copy(h, f)
	if err != nil {
		return nil, 0, fmt.Errorf("remux: hashing temporary file: %w", err)
	}
	return h.Sum(nil), n, nil
}

// Packets returns how many packets were muxed.
func (w *Writer) Packets() int { return w.written }

// Bytes returns the payload bytes muxed.
func (w *Writer) Bytes() int64 { return w.bytes }
