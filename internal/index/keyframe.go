// Package index builds the random-access keyframe index of a repaired
// track. The index only ever points at verified sync samples: declared
// keyframes whose payload is fully present on disk.
package index

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"sort"

	"mediaroute/internal/model"
)

// Entry is one random-access point on the shared 90 kHz clock.
type Entry struct {
	Clock  int64  // shared clock ticks of the sync sample
	Sample uint32 // sample index in the repaired track
	Offset uint64 // absolute file offset of the sample payload
	Size   uint32
}

// Index is a clock-ordered list of verified random-access points.
type Index struct {
	Entries []Entry
}

// Build indexes the verified sync samples of a repaired track.
func Build(t *model.Track) *Index {
	ix := &Index{Entries: make([]Entry, 0, len(t.Samples)/16+1)}
	for _, s := range t.Samples {
		if s.Keyframe && !s.Truncated {
			ix.Entries = append(ix.Entries, Entry{
				Clock:  s.Clock,
				Sample: s.Index,
				Offset: s.Offset,
				Size:   s.Size,
			})
		}
	}
	return ix
}

// Lookup returns the latest verified sync sample at or before clk.
func (ix *Index) Lookup(clk int64) (Entry, bool) {
	n := len(ix.Entries)
	i := sort.Search(n, func(i int) bool { return ix.Entries[i].Clock > clk })
	if i == 0 {
		return Entry{}, false
	}
	return ix.Entries[i-1], true
}

// Digest returns a stable SHA-256 hex digest of the index content.
// Repeat runs over identical input must produce identical digests.
func (ix *Index) Digest() string {
	h := sha256.New()
	var buf [32]byte
	binary.BigEndian.PutUint64(buf[0:8], uint64(len(ix.Entries)))
	h.Write(buf[0:8])
	for _, e := range ix.Entries {
		binary.BigEndian.PutUint64(buf[0:8], uint64(e.Clock))
		binary.BigEndian.PutUint32(buf[8:12], e.Sample)
		binary.BigEndian.PutUint64(buf[12:20], e.Offset)
		binary.BigEndian.PutUint32(buf[20:24], e.Size)
		h.Write(buf[0:24])
	}
	return hex.EncodeToString(h.Sum(nil))
}
