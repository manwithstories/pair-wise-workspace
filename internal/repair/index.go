package repair

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"hash"
	"sort"

	"archive.local/archive/internal/model"
)

// Entry is one random-access index row: the earliest decodable sample at or
// before a requested time, plus the byte range to read.
type Entry struct {
	Unified   int64  // keyframe presentation time on the unified clock
	Sample    int    // 0-based sample index of the keyframe
	ByteStart uint64 // file offset of the keyframe payload
	ByteEnd   uint64 // one past the last byte needed through the next keyframe
}

// TrackIndex is the index for one track.
type TrackIndex struct {
	TrackID uint32
	Entries []Entry
	// Sparse reports whether any request must back off to an earlier keyframe
	// to be decodable.
	Sparse bool
	// Exact maps every sample to the preceding keyframe entry, for seek
	// verification without rescanning.
	Backoff map[int]int
}

// Index is the full random-access index over the unified timeline.
type Index struct {
	Tracks   []TrackIndex
	Digest   string // hex sha256 over the canonical serialisation
	Samples  int
	Defects  []*model.Error
	Coverage model.Recoverable
}

// SeekResult describes the decode point chosen for a request.
type SeekResult struct {
	Track     uint32
	Request   int64
	Entry     Entry
	BackedOff bool  // true when the request landed on a sparse gap
	Distance  int64 // unified ticks between request and entry
}

// BuildIndex indexes the repaired samples of every track. Entries are the
// sync samples only; a request between keyframes resolves to the previous one.
func BuildIndex(tracks map[uint32][]Sample, order []uint32) (*Index, error) {
	idx := &Index{}
	idx.Coverage.FirstByte = -1
	for _, trackID := range order {
		samples := tracks[trackID]
		if len(samples) == 0 {
			return nil, &model.Error{Code: model.CodeNoDecodePoint, Track: trackID, Sample: -1,
				OffsetBytes: -1, Detail: "track has no samples to index"}
		}
		ti := TrackIndex{TrackID: trackID, Backoff: make(map[int]int, len(samples))}
		var prevEntry = -1
		for i := range samples {
			s := &samples[i]
			if s.Sync {
				start := s.Offset
				end := s.Offset + uint64(s.Size)
				ti.Entries = append(ti.Entries, Entry{
					Unified: s.Unified, Sample: s.Index, ByteStart: start, ByteEnd: end,
				})
				prevEntry = len(ti.Entries) - 1
			}
			if prevEntry >= 0 {
				ti.Backoff[s.Index] = prevEntry
			}
		}
		// A track with no sync samples gets one synthetic entry at the first
		// sample so seeking still has a defined answer.
		if len(ti.Entries) == 0 {
			s := &samples[0]
			last := &samples[len(samples)-1]
			ti.Entries = append(ti.Entries, Entry{Unified: s.Unified, Sample: s.Index,
				ByteStart: s.Offset, ByteEnd: last.Offset + uint64(last.Size)})
			for i := range samples {
				ti.Backoff[i] = 0
			}
		} else {
			// ByteEnd is the end of the last sample the entry can decode:
			// everything before the next keyframe is reachable from it.
			for k := range ti.Entries {
				nextStart := uint64(0)
				if k+1 < len(ti.Entries) {
					nextStart = ti.Entries[k+1].ByteStart
				} else {
					last := &samples[len(samples)-1]
					nextStart = last.Offset + uint64(last.Size)
				}
				ti.Entries[k].ByteEnd = nextStart
			}
			ti.Sparse = len(ti.Entries) < len(samples)
			for i := range samples {
				if _, ok := ti.Backoff[i]; !ok {
					// Samples before the first keyframe back off to entry 0.
					ti.Backoff[i] = 0
				}
			}
		}
		idx.Tracks = append(idx.Tracks, ti)
		idx.Samples += len(samples)
		if len(ti.Entries) > 0 {
			last := ti.Entries[len(ti.Entries)-1]
			if end := int64(last.ByteEnd); end > idx.Coverage.EndByte {
				idx.Coverage.EndByte = end
			}
		}
	}
	idx.Coverage.FirstSample = 0
	idx.Coverage.LastSample = idx.Samples
	digest, err := idx.canonicalDigest()
	if err != nil {
		return nil, err
	}
	idx.Digest = digest
	return idx, nil
}

// Resolve returns the decode point at or before t on the unified clock.
func (ti *TrackIndex) Resolve(t int64) (SeekResult, error) {
	if len(ti.Entries) == 0 {
		return SeekResult{}, &model.Error{Code: model.CodeNoDecodePoint, Track: ti.TrackID,
			Sample: -1, OffsetBytes: -1, Detail: "track index is empty"}
	}
	// Find the last entry whose start is <= t.
	i := sort.Search(len(ti.Entries), func(i int) bool { return ti.Entries[i].Unified > t })
	chosen := 0
	if i > 0 {
		chosen = i - 1
	}
	e := ti.Entries[chosen]
	res := SeekResult{Track: ti.TrackID, Request: t, Entry: e, Distance: t - e.Unified}
	// Any request that is not exactly a keyframe instant needs backoff, and
	// so does a request beyond the last keyframe.
	res.BackedOff = t != e.Unified
	return res, nil
}

// ResolveSample maps a sample index to its decodable entry.
func (ti *TrackIndex) ResolveSample(sample int) (Entry, bool) {
	e, ok := ti.Backoff[sample]
	if !ok {
		return Entry{}, false
	}
	return ti.Entries[e], true
}

// canonical bytes: stable across runs, independent of map iteration.
func (idx *Index) canonicalDigest() (string, error) {
	h := sha256.New()
	writeU32(h, idx.Coverage.FirstSample, idx.Coverage.LastSample)
	for i := range idx.Tracks {
		ti := &idx.Tracks[i]
		writeU64(h, uint64(ti.TrackID), uint64(len(ti.Entries)))
		for _, e := range ti.Entries {
			writeU64(h, uint64(e.Unified), uint64(e.Sample), e.ByteStart, e.ByteEnd)
		}
	}
	sum := h.Sum(nil)
	return hex.EncodeToString(sum), nil
}

func writeU32(h hash.Hash, a, b int) {
	var buf [8]byte
	binary.BigEndian.PutUint32(buf[0:], uint32(a))
	binary.BigEndian.PutUint32(buf[4:], uint32(b))
	h.Write(buf[:])
}

func writeU64(h hash.Hash, vals ...uint64) {
	var buf [8]byte
	for _, v := range vals {
		binary.BigEndian.PutUint64(buf[:], v)
		h.Write(buf[:])
	}
}

// Fingerprint returns the digest of a single track's entries. Cache entries
// store this per index block so a checkpoint can prove index completeness.
func (ti *TrackIndex) Fingerprint() string {
	h := sha256.New()
	writeU64(h, uint64(ti.TrackID), uint64(len(ti.Entries)))
	for _, e := range ti.Entries {
		writeU64(h, uint64(e.Unified), uint64(e.Sample), e.ByteStart, e.ByteEnd)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// BlockFingerprint returns the digest of entries [from,to), identifying one
// completed index block for checkpoint validation.
func (ti *TrackIndex) BlockFingerprint(from, to int) (string, error) {
	if from < 0 || to > len(ti.Entries) || from > to {
		return "", fmt.Errorf("repair: block [%d,%d) outside %d entries", from, to, len(ti.Entries))
	}
	h := sha256.New()
	writeU64(h, uint64(ti.TrackID), uint64(from), uint64(to))
	for _, e := range ti.Entries[from:to] {
		writeU64(h, uint64(e.Unified), uint64(e.Sample), e.ByteStart, e.ByteEnd)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
