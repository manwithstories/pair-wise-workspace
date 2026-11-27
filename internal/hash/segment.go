// Package hash computes deterministic per-segment digests.
//
// A segment digest covers exactly the bytes a consumer receives for that
// segment: normalized audio frames, metadata payloads and their timeline
// positions. The digest is therefore reproducible across runs as long as the
// pipeline is deterministic, which the recovery tests assert.
package hash

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"math"
)

// Segment identifies one output segment on the unified timeline.
type Segment struct {
	Index   int
	StartUS int64
	EndUS   int64
}

// Duration returns the segment length in microseconds.
func (s Segment) Duration() int64 { return s.EndUS - s.StartUS }

// Valid checks the segment boundaries.
func (s Segment) Valid() bool {
	return s.Index >= 0 && s.StartUS >= 0 && s.EndUS >= s.StartUS
}

// Report is one segment's digest record.
type Report struct {
	Segment Segment
	SHA256  string
	Bytes   int64
	Frames  int
	Records int
}

// Hasher accumulates a segment digest with length-prefixed framing so that
// concatenated payloads cannot collide (e.g. "ab"+"c" vs "a"+"bc").
type Hasher struct {
	buf    []byte
	h      hash.Hash
	bytes  int64
	frames int
	recs   int
}

// NewSegmentHasher starts a hasher for one segment.
func NewSegmentHasher(seg Segment) *Hasher {
	h := sha256.New()
	var hdr [24]byte
	binary.BigEndian.PutUint32(hdr[0:4], uint32(seg.Index))
	binary.BigEndian.PutUint64(hdr[8:16], uint64(seg.StartUS))
	binary.BigEndian.PutUint64(hdr[16:24], uint64(seg.EndUS))
	h.Write(hdr[:])
	return &Hasher{h: h}
}

// AddAudio folds normalized PCM samples (planar float32) into the digest.
func (h *Hasher) AddAudio(planes [][]float32) error {
	if len(planes) == 0 {
		return nil
	}
	n := len(planes[0])
	for _, p := range planes {
		if len(p) != n {
			return fmt.Errorf("hash: channel plane length mismatch")
		}
	}
	h.buf = h.buf[:0]
	h.buf = append(h.buf, 'A')
	var l [8]byte
	binary.BigEndian.PutUint32(l[:4], uint32(len(planes)))
	binary.BigEndian.PutUint32(l[4:], uint32(n))
	h.buf = append(h.buf, l[:]...)
	for _, p := range planes {
		for _, v := range p {
			binary.BigEndian.PutUint32(l[:4], f32bits(v))
			h.buf = append(h.buf, l[:4]...)
		}
	}
	h.h.Write(h.buf)
	h.bytes += int64(n * len(planes) * 4)
	h.frames += n
	return nil
}

// AddRecord folds one metadata record at a unified position.
func (h *Hasher) AddRecord(kind string, timeUS int64, payload []byte) error {
	if len(payload) > 1<<20 {
		return fmt.Errorf("hash: metadata payload %d bytes", len(payload))
	}
	h.buf = h.buf[:0]
	h.buf = append(h.buf, 'M')
	var l [12]byte
	binary.BigEndian.PutUint64(l[0:8], uint64(timeUS))
	binary.BigEndian.PutUint32(l[8:12], uint32(len(kind)))
	h.buf = append(h.buf, l[:]...)
	h.buf = append(h.buf, kind...)
	var n [4]byte
	binary.BigEndian.PutUint32(n[:], uint32(len(payload)))
	h.buf = append(h.buf, n[:]...)
	h.buf = append(h.buf, payload...)
	h.h.Write(h.buf)
	h.bytes += int64(len(payload))
	h.recs++
	return nil
}

// Digest finalizes the report.
func (h *Hasher) Digest(seg Segment) Report {
	return Report{
		Segment: seg,
		SHA256:  hex.EncodeToString(h.h.Sum(nil)),
		Bytes:   h.bytes,
		Frames:  h.frames,
		Records: h.recs,
	}
}

func f32bits(f float32) uint32 { return math.Float32bits(f) }

// DigestWriter hashes raw bytes with a fixed segment header; used by tests and
// by the fixture writer to prove byte-identical repeated runs.
type DigestWriter struct {
	h hash.Hash
	n int64
}

// NewDigestWriter returns a plain SHA-256 writer with a byte counter.
func NewDigestWriter() *DigestWriter { return &DigestWriter{h: sha256.New()} }

func (w *DigestWriter) Write(p []byte) (int, error) {
	n, err := w.h.Write(p)
	w.n += int64(n)
	return n, err
}

// Digest returns the hex digest.
func (w *DigestWriter) Digest() string { return hex.EncodeToString(w.h.Sum(nil)) }

// Bytes returns the number of bytes written.
func (w *DigestWriter) Bytes() int64 { return w.n }

var _ io.Writer = (*DigestWriter)(nil)

// Compare checks two reports for identical boundaries and digests.
func Compare(a, b []Report) error {
	if len(a) != len(b) {
		return fmt.Errorf("hash: segment count %d != %d", len(a), len(b))
	}
	for i := range a {
		if a[i].Segment != b[i].Segment {
			return fmt.Errorf("hash: segment %d boundaries differ: %+v vs %+v", i, a[i].Segment, b[i].Segment)
		}
		if a[i].SHA256 != b[i].SHA256 {
			return fmt.Errorf("hash: segment %d digest %s != %s", i, a[i].SHA256, b[i].SHA256)
		}
	}
	return nil
}
