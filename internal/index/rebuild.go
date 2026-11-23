// Package index rebuilds random-access keyframe indexes from parsed access
// units and repaired timestamps, validating every entry against the stream.
package index

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"

	"mediad/internal/clock"
	"mediad/internal/stream"
)

// Entry is one random-access point in the rebuilt index.
type Entry struct {
	Seq     uint64 // access-unit sequence number
	Time    int64  // unified media-clock time
	Offset  int64  // byte offset in the elementary stream
	Size    int
	Segment int // timeline segment the entry belongs to
}

// Index is a rebuilt random-access index.
type Index struct {
	Entries []Entry
	Skipped int // keyframes excluded (isolated or corrupted)
}

// Mismatch describes one index/stream disagreement.
type Mismatch struct {
	Seq    uint64
	Reason string
}

func (m Mismatch) String() string { return fmt.Sprintf("seq=%d: %s", m.Seq, m.Reason) }

// Rebuild joins access units with repaired timestamps by sequence number and
// keeps non-isolated, non-corrupted keyframes as random-access entries.
func Rebuild(units []stream.AccessUnit, fixed []clock.FixedSample) (*Index, []Mismatch) {
	idx := &Index{}
	var mism []Mismatch
	bySeq := make(map[uint64]clock.FixedSample, len(fixed))
	for _, f := range fixed {
		bySeq[f.Seq] = f
	}
	var prev Entry
	first := true
	for _, u := range units {
		f, ok := bySeq[uint64(u.Index)]
		if !ok {
			mism = append(mism, Mismatch{uint64(u.Index), "access unit has no repaired timestamp"})
			continue
		}
		if !u.Keyframe {
			continue
		}
		if u.Corrupted || f.Isolated || f.Time < 0 {
			idx.Skipped++
			continue
		}
		e := Entry{Seq: f.Seq, Time: f.Time, Offset: u.Offset, Size: u.Size, Segment: f.Segment}
		if !first {
			if e.Time <= prev.Time {
				mism = append(mism, Mismatch{e.Seq, "index time not strictly increasing"})
				continue
			}
			if e.Offset <= prev.Offset {
				mism = append(mism, Mismatch{e.Seq, "stream offset not strictly increasing"})
				continue
			}
		}
		idx.Entries = append(idx.Entries, e)
		prev, first = e, false
	}
	return idx, mism
}

// Validate cross-checks every index entry against the stream's keyframes.
func Validate(idx *Index, units []stream.AccessUnit) []Mismatch {
	key := map[int]stream.AccessUnit{}
	for _, u := range units {
		key[u.Index] = u
	}
	var mism []Mismatch
	for _, e := range idx.Entries {
		u, ok := key[int(e.Seq)]
		if !ok {
			mism = append(mism, Mismatch{e.Seq, "entry has no matching access unit"})
			continue
		}
		if !u.Keyframe {
			mism = append(mism, Mismatch{e.Seq, "entry does not point at a stream keyframe"})
		}
		if u.Offset != e.Offset {
			mism = append(mism, Mismatch{e.Seq, "entry offset disagrees with stream"})
		}
	}
	return mism
}

// Digest returns a deterministic SHA-256 digest of the index entries.
// Rebuilding the same input always yields the same digest.
func (idx *Index) Digest() string {
	h := sha256.New()
	var buf [8]byte
	for _, e := range idx.Entries {
		for _, v := range []int64{int64(e.Seq), e.Time, e.Offset, int64(e.Size), int64(e.Segment)} {
			binary.BigEndian.PutUint64(buf[:], uint64(v))
			h.Write(buf[:])
		}
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}
