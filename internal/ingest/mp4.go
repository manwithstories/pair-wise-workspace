package ingest

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/abema/go-mp4"
	"github.com/clearliu/fieldcut/internal/clock"
)

// MaxSegmentBytes hard-caps any single accepted segment payload. Larger
// inputs are rejected before parsing so a malicious node cannot drive
// allocations through a declared box size.
const MaxSegmentBytes = 64 << 20 // 64 MiB

// SegmentKind mirrors manifest.Kind without importing that package, so
// ingest stays a lower-level dependency.
type SegmentKind int

const (
	KindUnknown SegmentKind = iota
	KindInitSegment
	KindMediaSegment
)

// SegmentReport is the deterministic result of validating one segment.
// Fragments is the number of moof boxes found; PayloadBytes sums the
// declared mdat data; BoxCount includes every box at every depth.
type SegmentReport struct {
	Kind         SegmentKind
	BoxCount     int
	Fragments    int
	PayloadBytes int64
	HasSTYP      bool
	HasMOOV      bool
	HasSI        bool
	// Boxes lists every box type in traversal order (depth-first),
	// truncated to a fixed number for bounded reporting.
	Boxes []string
}

const maxReportedBoxes = 256

// ValidateSegment parses an init or media fMP4 segment with go-mp4 and
// checks the structural invariants fieldcut relies on. Truncated boxes,
// oversized declarations and missing mandatory boxes produce
// clock.ErrTruncatedBox / clock.ErrInvalidArgument wrapped errors.
func ValidateSegment(data []byte) (SegmentReport, error) {
	if len(data) == 0 {
		return SegmentReport{}, fmt.Errorf("%w: empty segment", clock.ErrTruncatedBox)
	}
	if len(data) > MaxSegmentBytes {
		return SegmentReport{}, fmt.Errorf("%w: segment %d bytes > %d",
			clock.ErrInvalidArgument, len(data), MaxSegmentBytes)
	}
	reader := bytes.NewReader(data)
	rep := SegmentReport{}
	fragmentTfhd := map[int]bool{}
	fragmentTrun := map[int]bool{}
	_, err := mp4.ReadBoxStructure(reader, func(h *mp4.ReadHandle) (interface{}, error) {
		bi := h.BoxInfo
		if bi.Size < bi.HeaderSize || bi.Offset+bi.Size > uint64(len(data)) {
			return nil, fmt.Errorf("%w: %s at offset %d size %d",
				clock.ErrTruncatedBox, bi.Type, bi.Offset, bi.Size)
		}
		rep.BoxCount++
		if len(rep.Boxes) < maxReportedBoxes {
			rep.Boxes = append(rep.Boxes, bi.Type.String())
		}
		switch bi.Type {
		case mp4.BoxTypeStyp():
			rep.HasSTYP = true
		case mp4.BoxTypeMoov():
			rep.HasMOOV = true
		case mp4.BoxTypeMoof():
			rep.Fragments++
		case mp4.BoxTypeTfhd():
			if !pathContains(h.Path, mp4.BoxTypeMoof()) {
				return nil, fmt.Errorf("%w: tfhd outside moof", clock.ErrTruncatedBox)
			}
			fragmentTfhd[rep.Fragments] = true
		case mp4.BoxTypeTrun():
			if !pathContains(h.Path, mp4.BoxTypeMoof()) {
				return nil, fmt.Errorf("%w: trun outside moof", clock.ErrTruncatedBox)
			}
			box, _, err := h.ReadPayload()
			if err != nil {
				return nil, fmt.Errorf("%w: trun payload: %v", clock.ErrTruncatedBox, err)
			}
			t, ok := box.(*mp4.Trun)
			if !ok || t.SampleCount == 0 {
				return nil, fmt.Errorf("%w: empty trun", clock.ErrTruncatedBox)
			}
			fragmentTrun[rep.Fragments] = true
		case mp4.BoxTypeMdat():
			rep.PayloadBytes += int64(bi.Size - bi.HeaderSize)
		case mp4.BoxTypeSidx():
			rep.HasSI = true
		}
		// Recurse into container boxes; leaf payloads are skipped.
		return h.Expand()
	})
	if err != nil {
		// Truncated headers and short reads surface from go-mp4 as EOF
		// class errors; normalize them to the project's sentinel.
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return SegmentReport{}, fmt.Errorf("%w: %v", clock.ErrTruncatedBox, err)
		}
		return SegmentReport{}, err
	}
	for frag := 1; frag <= rep.Fragments; frag++ {
		if !fragmentTfhd[frag] || !fragmentTrun[frag] {
			return SegmentReport{}, fmt.Errorf("%w: fragment %d missing tfhd/trun (tfhd=%v trun=%v)",
				clock.ErrTruncatedBox, frag, fragmentTfhd[frag], fragmentTrun[frag])
		}
	}
	switch {
	case rep.HasMOOV:
		rep.Kind = KindInitSegment
	case rep.Fragments > 0:
		rep.Kind = KindMediaSegment
	default:
		return rep, fmt.Errorf("%w: neither moov nor moof present", clock.ErrTruncatedBox)
	}
	if rep.Kind == KindMediaSegment && !rep.HasSTYP {
		return rep, fmt.Errorf("%w: media segment missing styp", clock.ErrTruncatedBox)
	}
	return rep, nil
}

// pathContains reports whether the given box type appears in the path.
func pathContains(path mp4.BoxPath, want mp4.BoxType) bool {
	for _, t := range path {
		if t.MatchWith(want) {
			return true
		}
	}
	return false
}

// ---- Minimal local box builder (fixtures only) -----------------------
//
// go-mp4's marshaller is oriented at full file trees; fieldcut's fixtures
// only need structurally valid byte shells, so a 20-line writer keeps the
// test data generator dependency-free and deterministic.

// BoxBuilder accumulates ISOBMFF boxes.
type BoxBuilder struct{ buf bytes.Buffer }

// Box appends a complete box with the given 4-char type and payload.
func (b *BoxBuilder) Box(typ string, payload []byte) *BoxBuilder {
	if len(typ) != 4 {
		panic("box type must be 4 chars")
	}
	hdr := make([]byte, 8)
	binary.BigEndian.PutUint32(hdr, uint32(8+len(payload)))
	copy(hdr[4:], typ)
	b.buf.Write(hdr)
	b.buf.Write(payload)
	return b
}

// Container appends a box whose payload is another builder's bytes.
func (b *BoxBuilder) Container(typ string, inner *BoxBuilder) *BoxBuilder {
	return b.Box(typ, inner.Bytes())
}

// Bytes returns the serialized segment.
func (b *BoxBuilder) Bytes() []byte { out := b.buf.Bytes(); return out }

// Len reports the current segment size.
func (b *BoxBuilder) Len() int { return b.buf.Len() }

// Truncate cuts the segment at n bytes to emulate a slow archive disk
// that delivered only part of a fragment.
func (b *BoxBuilder) Truncate(n int) []byte {
	full := b.buf.Bytes()
	if n > len(full) {
		n = len(full)
	}
	out := make([]byte, n)
	copy(out, full[:n])
	return out
}

// Ensure bytes.Reader stays an io.ReadSeeker for future extensions.
var _ io.ReadSeeker = (*bytes.Reader)(nil)
