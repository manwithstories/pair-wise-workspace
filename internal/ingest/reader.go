// Package ingest provides bounded input reading: every byte read is
// counted against a budget and an over-limit input is rejected rather
// than buffered without bound.
package ingest

import (
	"bufio"
	"errors"
	"fmt"
	"io"
)

// ErrLimitExceeded is returned when the configured byte budget is hit.
var ErrLimitExceeded = errors.New("ingest: input exceeds configured byte budget")

// Reader wraps an io.Reader with a hard byte budget and tracks the
// cumulative accepted byte count for memory accounting.
type Reader struct {
	src    *bufio.Reader
	limit  int64
	read   int64
	peak   int64
	closed bool
}

// NewReader wraps src. A limit <= 0 is treated as invalid.
func NewReader(src io.Reader, limitBytes int64) (*Reader, error) {
	if limitBytes <= 0 {
		return nil, fmt.Errorf("ingest: limit must be positive, got %d", limitBytes)
	}
	return &Reader{
		src:   bufio.NewReaderSize(src, 64*1024),
		limit: limitBytes,
	}, nil
}

// Read implements io.Reader with budget enforcement.
func (r *Reader) Read(p []byte) (int, error) {
	if r.closed {
		return 0, io.ErrClosedPipe
	}
	remaining := r.limit - r.read
	if remaining <= 0 {
		// Budget already exhausted. Probe the source once: a readable
		// byte proves the input is over-budget; EOF means it ended
		// exactly on the limit and the caller sees a clean EOF.
		if _, perr := r.src.Peek(1); perr == nil {
			return 0, ErrLimitExceeded
		}
		return 0, io.EOF
	}
	if int64(len(p)) > remaining {
		p = p[:remaining]
	}
	n, err := r.src.Read(p)
	r.read += int64(n)
	if r.read > r.peak {
		r.peak = r.read
	}
	return n, err
}

// ReadAtMost reads up to maxBytes in one bounded allocation. It is
// intended for box-sized reads whose upper bound is known.
func (r *Reader) ReadAtMost(maxBytes int) ([]byte, error) {
	if maxBytes < 0 {
		return nil, fmt.Errorf("ingest: negative read size %d", maxBytes)
	}
	buf := make([]byte, 0, maxBytes)
	chunk := make([]byte, min(32*1024, max(1, maxBytes)))
	for len(buf) < maxBytes {
		n, err := r.Read(chunk)
		buf = append(buf, chunk[:n]...)
		if err != nil {
			if errors.Is(err, io.EOF) && len(buf) > 0 {
				return buf, io.EOF
			}
			return buf, err
		}
		if n == 0 {
			break
		}
	}
	return buf, nil
}

// BytesRead reports the cumulative accepted byte count.
func (r *Reader) BytesRead() int64 { return r.read }

// PeakBytes reports the highest accepted cumulative byte count.
func (r *Reader) PeakBytes() int64 { return r.peak }

// Limit reports the configured budget.
func (r *Reader) Limit() int64 { return r.limit }

// Close marks the reader closed; subsequent reads fail.
func (r *Reader) Close() { r.closed = true }
