package fingerprint

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"sort"
)

// Segment is the content-addressable unit produced by the cutter.
type Segment struct {
	ID      string
	Track   string
	StartMs float64
	EndMs   float64
	Samples int
	Silent  bool // fully silent per the silence report
	Payload []byte
}

// Fingerprint hashes payload together with duration and sample count,
// so a bare payload-digest collision can never merge segments whose
// timing differs.
func Fingerprint(s Segment) string {
	h := sha256.New()
	h.Write(s.Payload)
	var meta [16]byte
	binary.BigEndian.PutUint64(meta[0:8], uint64(s.EndMs-s.StartMs+0.5))
	binary.BigEndian.PutUint64(meta[8:16], uint64(s.Samples))
	h.Write(meta[:])
	return hex.EncodeToString(h.Sum(nil))
}

// Duplicate is one group of segments with identical verified content.
type Duplicate struct {
	Hash       string
	SegmentIDs []string
	Track      string
	TimeRanges [][2]float64
}

// FindDuplicates groups segments by fingerprint. Short or fully silent
// segments are never merged on the digest alone: they join a duplicate
// group only after an exact byte-for-byte payload comparison against
// the group representative.
func FindDuplicates(segs []Segment, minDurationMs float64) []Duplicate {
	type group struct {
		rep     Segment
		members []Segment
	}
	groups := map[string]*group{}
	order := []string{}
	for _, s := range segs {
		fp := Fingerprint(s)
		g, ok := groups[fp]
		if !ok {
			groups[fp] = &group{rep: s, members: []Segment{s}}
			order = append(order, fp)
			continue
		}
		short := s.EndMs-s.StartMs < minDurationMs
		if short || s.Silent {
			// Digest match is not enough for short or silent
			// segments: require exact payload equality.
			if !bytes.Equal(s.Payload, g.rep.Payload) {
				continue // collision: stays its own segment
			}
		}
		g.members = append(g.members, s)
	}
	var dups []Duplicate
	for _, fp := range order {
		g := groups[fp]
		if len(g.members) < 2 {
			continue
		}
		d := Duplicate{Hash: fp[:16], Track: g.rep.Track}
		for _, m := range g.members {
			d.SegmentIDs = append(d.SegmentIDs, m.ID)
			d.TimeRanges = append(d.TimeRanges, [2]float64{m.StartMs, m.EndMs})
		}
		sort.Strings(d.SegmentIDs)
		dups = append(dups, d)
	}
	sort.Slice(dups, func(i, j int) bool { return dups[i].Hash < dups[j].Hash })
	return dups
}
