package pipeline

import (
	"context"
	"errors"
	"fmt"
	"io"
)

// CanceledError marks a run stopped by the caller's context. The CLI maps it
// to exit 1 because no committed output exists.
type CanceledError struct{ Err error }

func (e *CanceledError) Error() string {
	if e.Err == nil {
		return ErrCanceled.Error()
	}
	return fmt.Sprintf("%v: %v", ErrCanceled, e.Err)
}

func (e *CanceledError) Unwrap() error { return e.Err }

// cancelReader aborts a read as soon as the context ends, so parsing a large
// or hostile stream cannot outlive cancellation.
type cancelReader struct {
	ctx context.Context
	r   io.Reader
}

func newCancelReader(ctx context.Context, r io.Reader) *cancelReader {
	return &cancelReader{ctx: ctx, r: r}
}

func (c *cancelReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := c.r.Read(p)
	if err != nil && c.ctx.Err() != nil {
		return n, c.ctx.Err()
	}
	return n, err
}

// byteSource is an io.Reader over an in-memory container.
type byteSource struct {
	data []byte
	off  int
}

func newByteSource(b []byte) *byteSource { return &byteSource{data: b} }

func (b *byteSource) Read(p []byte) (int, error) {
	if b.off >= len(b.data) {
		return 0, io.EOF
	}
	n := copy(p, b.data[b.off:])
	b.off += n
	return n, nil
}

// closeAll releases every buffer so cancellation leaves no queue or worker
// alive behind it.
func closeAll[T interface{ Close() }](m map[uint64]T) {
	for _, v := range m {
		v.Close()
	}
}

// isCanceled reports whether err came from context cancellation.
func isCanceled(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}
