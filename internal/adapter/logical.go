package adapter

import (
	"crypto/sha256"
	"fmt"
	"hash"
	"io"

	"mediacore/internal/media"
)

// LogicalSource synthesizes the mdat payload of a generated fixture on
// demand. A 1 GiB logical stream therefore needs no disk space and no
// resident memory: ReadAt fills the requested window from a deterministic
// formula. Structural byte ranges come from the generated prefix.
type LogicalSource struct {
	prefix     []byte
	payloadLen int64
	name       string
	// failures records offsets where the source must report a short read,
	// used to model a damaged medium.
	failFrom int64
}

// NewLogicalSource wraps a generated MP4 layout.
func NewLogicalSource(layout *MP4Layout, name string) *LogicalSource {
	return &LogicalSource{prefix: layout.Prefix, payloadLen: layout.PayloadSize, name: name, failFrom: -1}
}

// NewMixedSource wraps either generated layout. Essence holes are filled with
// the same deterministic formula, so a fixture is reproducible and a single
// implementation covers both containers.
func NewMixedSource(mxf *MXFLayout, mp4 *MP4Layout, name string) *LogicalSource {
	switch {
	case mxf != nil:
		// The MXF subset materializes its essence holes in the prefix, so the
		// prefix is the whole file.
		return &LogicalSource{prefix: mxf.Prefix, payloadLen: 0, name: name, failFrom: -1}
	case mp4 != nil:
		return NewLogicalSource(mp4, name)
	default:
		return &LogicalSource{name: name, failFrom: -1}
	}
}

func (s *LogicalSource) Size() int64  { return int64(len(s.prefix)) + s.payloadLen }
func (s *LogicalSource) Name() string { return s.name }

// ReadAt serves bytes from the structural prefix or from the synthesized
// payload. It never allocates proportional to the source size. Readers that
// request bytes spanning the prefix/hole boundary get the hole filled in, so
// no caller can observe an artificial EOF inside the file.
func (s *LogicalSource) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 || off >= s.Size() {
		return 0, io.EOF
	}
	if len(p) == 0 {
		return 0, nil
	}
	if s.failFrom >= 0 && off+int64(len(p)) > s.failFrom {
		avail := s.failFrom - off
		if avail <= 0 {
			return 0, io.EOF
		}
		p = p[:avail]
	}
	if off+int64(len(p)) > s.Size() {
		p = p[:s.Size()-off]
	}
	n := 0
	if off < int64(len(s.prefix)) {
		c := copy(p, s.prefix[off:])
		n += c
	}
	if n < len(p) {
		fillPayload(p[n:], off+int64(n)-int64(len(s.prefix)))
		n = len(p)
	}
	return n, nil
}

// fillPayload produces deterministic, non-repeating-looking bytes without
// any allocation beyond the destination slice.
func fillPayload(dst []byte, payloadOff int64) {
	for i := range dst {
		v := uint64(payloadOff + int64(i))
		// splitmix64 finalizer, truncated to a byte
		v += 0x9e3779b97f4a7c15
		v = (v ^ (v >> 30)) * 0xbf58476d1ce4e5b9
		v = (v ^ (v >> 27)) * 0x94d049bb133111eb
		dst[i] = byte(v ^ (v >> 31))
	}
}

// PayloadHash computes SHA-256 over the synthesized sample stream of one
// track, reading in bounded chunks. size is the payload prefix length used to
// derive the stream; the result is used by integrity tests.
func PayloadHash(src media.Source, samples []media.Sample, bufSize int) ([]byte, error) {
	if bufSize <= 0 {
		bufSize = 64 << 10
	}
	buf := make([]byte, bufSize)
	var h hash.Hash = sha256.New()
	for _, s := range samples {
		remaining := s.Size
		off := s.Offset
		for remaining > 0 {
			chunk := int64(len(buf))
			if chunk > remaining {
				chunk = remaining
			}
			n, err := src.ReadAt(buf[:chunk], off)
			if n > 0 {
				h.Write(buf[:n])
			}
			if err != nil && (err != io.EOF || n == 0) {
				if err != io.EOF {
					return nil, media.NewError(media.ClassIO, "cannot hash sample stream").WithCause(err)
				}
				return nil, media.NewError(media.ClassTruncated,
					"sample at offset %d ends past the input", off).WithOffset(off)
			}
			if n == 0 {
				return nil, media.NewError(media.ClassTruncated,
					"sample at offset %d ends past the input", off).WithOffset(off)
			}
			off += int64(n)
			remaining -= int64(n)
		}
	}
	return h.Sum(nil), nil
}

func (s *LogicalSource) String() string {
	return fmt.Sprintf("logical source %s (%d bytes, %d structural)", s.name, s.Size(), len(s.prefix))
}
