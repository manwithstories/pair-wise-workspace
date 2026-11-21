// Package index builds and queries a keyframe index. Matroska Cues are used
// when present and well-formed; otherwise the block headers are scanned to
// rebuild the index. A truncated or corrupt block can never produce an index
// entry, because entries are only emitted for fully decoded blocks.
package index

import (
	"bytes"
	"fmt"
	"sort"

	"archivecheck.local/archivecheck/internal/ebml"
	"archivecheck.local/archivecheck/internal/fault"
)

// Entry locates one keyframe. Offsets are intentionally not called "source":
// when the index comes from decoded blocks they point into the Matroska
// input, and when it comes from a finished archive copy they point into the
// MP4 output. Callers know which index they hold.
type Entry struct {
	// Track is the Matroska track number.
	Track uint64
	// Tick is the absolute timestamp in Segment ticks.
	Tick int64
	// SourceOffset is the absolute offset of the frame payload, in whichever
	// file this index describes.
	SourceOffset int64
	// ClusterOffset is the absolute offset of the enclosing Cluster.
	ClusterOffset int64
	// FrameIndex is the frame index inside a laced block.
	FrameIndex int
	// OutputSample is the output sample number (1-based) within its track.
	OutputSample int
	// FromCues reports whether the file's Cues element corroborates the entry.
	FromCues bool
}

// Index is a per-track keyframe index.
type Index struct {
	Entries []Entry
	// Rebuilt reports that no usable Cues were present and the index was
	// reconstructed by scanning blocks.
	Rebuilt bool
	// CueEntries is the number of CuePoint elements found in the source.
	CueEntries int
	// DiscardedCues counts cue points rejected by validation.
	DiscardedCues int
}

// CuePoint is one parsed Cues/CuePoint.
type CuePoint struct {
	Tick int64
	// Track and ClusterPosition are from the first CueTrackPositions child.
	Track           uint64
	ClusterPosition int64
	BlockNumber     uint64
}

// ByTrack returns entries for one track ordered by timestamp.
func (ix *Index) ByTrack(track uint64) []Entry {
	var out []Entry
	for _, e := range ix.Entries {
		if e.Track == track {
			out = append(out, e)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Tick != out[j].Tick {
			return out[i].Tick < out[j].Tick
		}
		return out[i].SourceOffset < out[j].SourceOffset
	})
	return out
}

// AtOrBefore returns the last keyframe at or before tick for a track.
func (ix *Index) AtOrBefore(track uint64, tick int64) (Entry, bool) {
	entries := ix.ByTrack(track)
	best := -1
	for i := range entries {
		if entries[i].Tick <= tick {
			best = i
			continue
		}
		break
	}
	if best < 0 {
		return Entry{}, false
	}
	return entries[best], true
}

// Hit reports whether querying tick returns the expected sample offset. It is
// the unit used by the keyframe hit-rate metric.
func (ix *Index) Hit(track uint64, tick int64, wantOffset int64) bool {
	e, ok := ix.AtOrBefore(track, tick)
	return ok && e.SourceOffset == wantOffset
}

// Sample is one decoded frame as seen by the index builder.
type Sample struct {
	TrackNumber   uint64
	Tick          int64
	Keyframe      bool
	SourceOffset  int64
	ClusterOffset int64
	FrameIndex    int
}

// FromSamples builds a keyframe index from decoded samples in file order.
// Only completely decoded blocks reach this function, so a truncated block
// can never create a phantom entry.
func FromSamples(samples []Sample) Index {
	ix := Index{Rebuilt: true}
	perTrack := map[uint64]int{}
	for _, s := range samples {
		perTrack[s.TrackNumber]++
		if !s.Keyframe {
			continue
		}
		ix.Entries = append(ix.Entries, Entry{
			Track:         s.TrackNumber,
			Tick:          s.Tick,
			SourceOffset:  s.SourceOffset,
			ClusterOffset: s.ClusterOffset,
			FrameIndex:    s.FrameIndex,
			OutputSample:  perTrack[s.TrackNumber],
		})
	}
	return ix
}

// ParseCues parses a Cues element payload.
func ParseCues(payload []byte) ([]CuePoint, error) {
	r := ebml.NewReader(bytes.NewReader(payload))
	end := int64(len(payload))
	var points []CuePoint
	for r.Offset() < end {
		e, err := r.Next(end)
		if err != nil {
			return nil, err
		}
		if e.ID != ebml.IDCuePoint {
			if err := r.Skip(e); err != nil {
				return nil, err
			}
			continue
		}
		cp, ok, err := parseCuePoint(r, e)
		if err != nil {
			return nil, err
		}
		if ok {
			points = append(points, cp)
		}
	}
	return points, nil
}

func parseCuePoint(r *ebml.Reader, e ebml.Element) (CuePoint, bool, error) {
	var cp CuePoint
	var haveTick, haveTrackPos bool
	for r.Offset() < e.End {
		c, err := r.Next(e.End)
		if err != nil {
			return cp, false, err
		}
		switch c.ID {
		case ebml.IDCueTime:
			v, err := r.ReadUint(c)
			if err != nil {
				return cp, false, err
			}
			cp.Tick = int64(v)
			haveTick = true
		case ebml.IDCueTrackPositions:
			track, pos, block, err := parseCueTrackPositions(r, c)
			if err != nil {
				return cp, false, err
			}
			if !haveTrackPos {
				cp.Track, cp.ClusterPosition, cp.BlockNumber = track, pos, block
				haveTrackPos = true
			}
		default:
			if err := r.Skip(c); err != nil {
				return cp, false, err
			}
		}
	}
	return cp, haveTick && haveTrackPos, nil
}

func parseCueTrackPositions(r *ebml.Reader, e ebml.Element) (track uint64, position int64, block uint64, err error) {
	if e.UnknownSize {
		return 0, 0, 0, fault.AtElement(fault.StageIndex, e.Offset, "CueTrackPositions", fault.ErrMalformed)
	}
	for r.Offset() < e.End {
		c, err := r.Next(e.End)
		if err != nil {
			return 0, 0, 0, err
		}
		switch c.ID {
		case ebml.IDCueTrack:
			v, err := r.ReadUint(c)
			if err != nil {
				return 0, 0, 0, err
			}
			track = v
		case ebml.IDCueClusterPosition:
			v, err := r.ReadUint(c)
			if err != nil {
				return 0, 0, 0, err
			}
			if v > 1<<62 {
				return 0, 0, 0, fault.AtElement(fault.StageIndex, c.Offset, "CueClusterPosition", fault.ErrOversized)
			}
			position = int64(v)
		case ebml.IDCueBlockNumber:
			v, err := r.ReadUint(c)
			if err != nil {
				return 0, 0, 0, err
			}
			block = v
		default:
			if err := r.Skip(c); err != nil {
				return 0, 0, 0, err
			}
		}
	}
	return track, position, block, nil
}

// AttachCues correlates parsed cue points with a sample-derived index. A cue
// is accepted only when it names a cluster that actually exists, so a corrupt
// Cues element cannot fabricate a hit. It returns the number of rejected cues.
func (ix *Index) AttachCues(points []CuePoint, segmentDataStart int64, clusterOffsets []int64) int {
	ix.CueEntries = len(points)
	clusterSet := make(map[int64]bool, len(clusterOffsets))
	for _, off := range clusterOffsets {
		clusterSet[off] = true
	}
	byTrack := map[uint64]map[int64]int{} // track -> tick -> entry index
	for i := range ix.Entries {
		e := &ix.Entries[i]
		if byTrack[e.Track] == nil {
			byTrack[e.Track] = map[int64]int{}
		}
		if _, ok := byTrack[e.Track][e.Tick]; !ok {
			byTrack[e.Track][e.Tick] = i
		}
	}
	accepted := 0
	for _, p := range points {
		abs := segmentDataStart + p.ClusterPosition
		if p.ClusterPosition < 0 || !clusterSet[abs] {
			ix.DiscardedCues++
			continue
		}
		ticks, ok := byTrack[p.Track]
		if !ok {
			ix.DiscardedCues++
			continue
		}
		idx, ok := ticks[p.Tick]
		if !ok {
			// Point at a tick where no complete keyframe exists (for example a
			// cue into a truncated cluster): do not invent an entry.
			ix.DiscardedCues++
			continue
		}
		ix.Entries[idx].FromCues = true
		accepted++
	}
	if accepted == 0 {
		ix.Rebuilt = true
	} else {
		ix.Rebuilt = false
	}
	return accepted
}

// Validate checks an index for internal consistency: offsets must lie inside
// the input and timestamps must not decrease per track.
func (ix *Index) Validate(inputSize int64) error {
	last := map[uint64]int64{}
	for _, e := range ix.Entries {
		if e.SourceOffset < 0 || e.SourceOffset >= inputSize {
			return fault.AtElement(fault.StageIndex, e.SourceOffset, "CuePoint",
				fmt.Errorf("%w: cue offset outside input", fault.ErrMalformed)).OnTrack(e.Track)
		}
		if e.Tick < last[e.Track] {
			return fault.AtElement(fault.StageIndex, e.SourceOffset, "CuePoint",
				fmt.Errorf("%w: cue timestamps regress for track %d", fault.ErrMalformed, e.Track)).OnTrack(e.Track)
		}
		last[e.Track] = e.Tick
	}
	return nil
}
