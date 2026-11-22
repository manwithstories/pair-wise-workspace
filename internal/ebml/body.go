package ebml

import (
	"mediapack/internal/clock"
)

// ReadBody reads the full body of a finite element, enforcing a byte
// budget: requesting more than maxBytes yields clock.CatOversized
// without allocating. Used for small fixed-size elements (timecodes,
// track numbers); block payloads use ReadBodyLimited streaming instead.
func (r *Reader) ReadBody(el *Element, maxBytes int64) ([]byte, error) {
	if el.Unknown {
		return nil, clock.NewError(clock.CatBadPath, "cannot buffer unknown-length element %x", el.ID)
	}
	remaining := el.EndOff - r.off
	if remaining < 0 {
		return nil, clock.AtError(clock.CatBadPath, r.off, "reader already past element end")
	}
	if remaining > maxBytes {
		return nil, clock.AtError(clock.CatOversized, el.HeaderOff,
			"element body %d bytes exceeds read budget %d", remaining, maxBytes)
	}
	buf, err := r.readByteN(int(remaining))
	if err != nil {
		return nil, err
	}
	return buf, nil
}

// ReadBodyLimited streams at most limit bytes of el's body through fn.
// The callback receives each chunk; total delivered bytes never exceed
// the limit or the element's declared size. A truncated stream produces
// clock.CatTruncated anchored at the first missing byte.
func (r *Reader) ReadBodyLimited(el *Element, limit int64, fn func(chunk []byte) error) error {
	if el.Unknown {
		return clock.NewError(clock.CatBadPath, "streaming unknown-length element %x needs a container walk", el.ID)
	}
	remaining := el.EndOff - r.off
	if remaining < 0 {
		return clock.AtError(clock.CatBadPath, r.off, "reader already past element end")
	}
	if remaining > limit {
		return clock.AtError(clock.CatOversized, el.HeaderOff,
			"body of %d bytes exceeds streaming limit %d", remaining, limit)
	}
	var buf [4096]byte
	for remaining > 0 {
		chunk := remaining
		if chunk > int64(len(buf)) {
			chunk = int64(len(buf))
		}
		b, err := r.readByteN(int(chunk))
		if err != nil {
			return err
		}
		if err := fn(b); err != nil {
			return err
		}
		remaining -= chunk
	}
	return nil
}

// ReadRaw reads exactly n bytes from the underlying stream, advancing
// the offset. It exists for checksum/fingerprint consumers that need
// raw input ranges rather than element semantics.
func (r *Reader) ReadRaw(n int) ([]byte, error) {
	if n < 0 {
		return nil, clock.NewError(clock.CatBadPath, "negative raw read length %d", n)
	}
	return r.readByteN(n)
}

// EOFAfterUnknown should be called by a container walk when a Next call
// on an unknown-length scope returns io.EOF: it distinguishes a clean
// live-stream end from truncation of a finite parent.
func (r *Reader) EOFAfterUnknown() bool {
	for _, s := range r.stack {
		if s.bodyEnd < 0 {
			return true
		}
	}
	return false
}
