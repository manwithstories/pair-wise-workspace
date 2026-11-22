// Package index builds and verifies the random-access index for a
// repaired timeline. It mirrors the ISO/IEC 14496-12 sidx reference
// layout but stays self-contained: entries are plain Go structs that can
// also be serialized into a real sidx box by the builder. Index output
// is canonical and deterministic, so rerunning the same input always
// yields the same SHA-256 digest.
package index

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"sort"

	"github.com/clearliu/fieldcut/internal/clock"
)

// MaxEntries hard-caps index size regardless of timeline length.
const MaxEntries = 1_000_000

// Entry is one random-access reference: every entry starts at a keyframe
// boundary and is independently seekable.
type Entry struct {
	TrackID   int
	Sequence  int
	// Start/End are on the unified 90 kHz axis.
	Start     clock.Ticks
	End       clock.Ticks
	// Offset/Size locate the fragment bytes inside the concatenated
	// segment payload (or an archive object in a real deployment).
	Offset    int64
	Size      int64
	// Untrusted entries are retained but flagged; seeks are allowed to
	// land on them only with an explicit override.
	Untrusted bool
}

// Index is the canonical, sorted random-access structure.
type Index struct {
	Timescale int64
	Entries   []Entry
}

// Builder accumulates entries and enforces structural invariants before
// materializing an Index.
type Builder struct {
	entries   []Entry
	seen      map[[2]int]bool // (track, sequence) dedupe
	nextOff   int64
}

// NewBuilder returns an empty builder.
func NewBuilder() *Builder {
	return &Builder{seen: map[[2]int]bool{}}
}

// Add validates and appends one fragment reference.
func (b *Builder) Add(e Entry) error {
	if e.End < e.Start {
		return fmt.Errorf("%w: entry end %d before start %d", clock.ErrInvalidArgument, e.End, e.Start)
	}
	if e.Size < 0 || e.Offset < 0 {
		return fmt.Errorf("%w: negative offset/size", clock.ErrInvalidArgument)
	}
	key := [2]int{e.TrackID, e.Sequence}
	if b.seen[key] {
		return fmt.Errorf("%w: track %d seq %d indexed twice", clock.ErrDuplicateSeq, e.TrackID, e.Sequence)
	}
	if len(b.entries) >= MaxEntries {
		return fmt.Errorf("%w: more than %d index entries", clock.ErrManifestTooLarge, MaxEntries)
	}
	b.seen[key] = true
	b.entries = append(b.entries, e)
	return nil
}

// Build returns the canonical index: entries sorted by (Start, TrackID,
// Sequence) with offsets regenerated deterministically, so input arrival
// order cannot influence the digest.
func (b *Builder) Build() (*Index, error) {
	out := &Index{Timescale: clock.Rate, Entries: make([]Entry, len(b.entries))}
	copy(out.Entries, b.entries)
	sort.SliceStable(out.Entries, func(i, j int) bool {
		a, c := out.Entries[i], out.Entries[j]
		if a.Start != c.Start {
			return a.Start < c.Start
		}
		if a.TrackID != c.TrackID {
			return a.TrackID < c.TrackID
		}
		return a.Sequence < c.Sequence
	})
	var off int64
	for i := range out.Entries {
		out.Entries[i].Offset = off
		off += out.Entries[i].Size
	}
	if err := Verify(out); err != nil {
		return nil, err
	}
	return out, nil
}

// Verify checks the invariants a seek relies on: non-empty, sorted,
// non-overlapping per track, and offsets forming a contiguous layout.
func Verify(idx *Index) error {
	if idx == nil || len(idx.Entries) == 0 {
		return fmt.Errorf("%w: empty index", clock.ErrInvalidArgument)
	}
	if idx.Timescale != clock.Rate {
		return fmt.Errorf("%w: timescale %d != %d", clock.ErrInvalidArgument, idx.Timescale, clock.Rate)
	}
	lastEnd := map[int]clock.Ticks{}
	var wantOff int64
	for i, e := range idx.Entries {
		if i > 0 {
			prev := idx.Entries[i-1]
			if e.Start < prev.Start {
				return fmt.Errorf("%w: entries not sorted at %d", clock.ErrInvalidArgument, i)
			}
		}
		if e.Offset != wantOff {
			return fmt.Errorf("%w: offset gap at entry %d", clock.ErrInvalidArgument, i)
		}
		wantOff += e.Size
		if end, ok := lastEnd[e.TrackID]; ok && e.Start < end {
			return fmt.Errorf("%w: overlapping entries on track %d at seq %d",
				clock.ErrInvalidArgument, e.TrackID, e.Sequence)
		}
		lastEnd[e.TrackID] = e.End
	}
	return nil
}

// Digest returns the canonical SHA-256 over the serialized sidx payload.
// Two indexes with identical logical entries always match regardless of
// the order entries were added.
func (idx *Index) Digest() (string, error) {
	raw, err := idx.MarshalSIDX()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

// MarshalSIDX serializes the index into a minimal big-endian sidx box:
// 8-byte box header, version/flags, reference id/timescale, earliest
// presentation time/first offset, reserved/spacing, then one reference
// per entry (reference_type=0 media reference, referenced_size,
// subsegment_duration, SAP flags). All integers are fixed width so the
// encoding is canonical.
func (idx *Index) MarshalSIDX() ([]byte, error) {
	if err := Verify(idx); err != nil {
		return nil, err
	}
	const (
		fullBoxHdr = 4 // version + flags
		fixedBody  = 4 + 4 + 4 + 4 + 2 + 2
		refSize    = 4 + 4 + 4
	)
	payloadLen := fullBoxHdr + fixedBody + len(idx.Entries)*refSize
	box := make([]byte, 8+payloadLen)
	// Box header.
	binary.BigEndian.PutUint32(box[0:4], uint32(8+payloadLen))
	copy(box[4:8], "sidx")
	p := box[8:]
	// version=0, flags=0.
	p[0], p[1], p[2], p[3] = 0, 0, 0, 0
	p = p[4:]
	binary.BigEndian.PutUint32(p[0:4], 1) // reference_ID
	binary.BigEndian.PutUint32(p[4:8], uint32(idx.Timescale))
	binary.BigEndian.PutUint32(p[8:12], uint32(idx.Entries[0].Start))
	binary.BigEndian.PutUint32(p[12:16], 0) // first_offset
	binary.BigEndian.PutUint16(p[16:18], 0) // reserved
	binary.BigEndian.PutUint16(p[18:20], uint16(len(idx.Entries)))
	p = p[20:]
	for _, e := range idx.Entries {
		// reference_type(1b)=0 + referenced_size(31b).
		refSizeField := uint32(e.Size)
		if e.Untrusted {
			refSizeField |= 1 << 31 // SAP marking bit reused as untrusted flag
		}
		binary.BigEndian.PutUint32(p[0:4], refSizeField)
		binary.BigEndian.PutUint32(p[4:8], uint32(e.End-e.Start))
		var sap uint32
		if e.Untrusted {
			sap = 1
		}
		binary.BigEndian.PutUint32(p[8:12], sap)
		p = p[12:]
	}
	return box, nil
}
