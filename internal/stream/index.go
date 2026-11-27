package stream

import (
	"fmt"
	"io"

	"avflow/internal/model"
)

// KeyframeIndexEntry locates one keyframe on the nanosecond timeline.
type KeyframeIndexEntry struct {
	Seq   uint64
	PTSNS int64
}

// Index is the parsed-and-validated view of a stream: packet sequence per
// track, keyframe locations, every detected fault and the first fault on the
// global packet order (the primary localization result).
type Index struct {
	Tracks       []TrackDesc
	Packets      []*Packet
	ByTrack      map[uint32][]*Packet
	Keyframes    map[uint32][]KeyframeIndexEntry
	Faults       []*model.Fault
	FirstFault   *model.Fault
	// FirstCorruptGlobal is the packet order index at which a bad-CRC or
	// truncated packet first appeared; downstream stages crop output there.
	FirstCorruptGlobal int
	BadCRCSeqs         map[uint32]map[uint64]bool
}

// BuildIndex consumes a Reader to completion and runs every packet-level
// semantic check. Reader-level failures (truncation, limits) are captured as
// faults and stop the scan; the packets parsed so far remain available.
func BuildIndex(r *Reader) (*Index, error) {
	idx := &Index{
		Tracks:             append([]TrackDesc(nil), r.Tracks()...),
		ByTrack:            map[uint32][]*Packet{},
		Keyframes:          map[uint32][]KeyframeIndexEntry{},
		BadCRCSeqs:         map[uint32]map[uint64]bool{},
		FirstCorruptGlobal: -1,
	}
	lastPTS := map[uint32]int64{}
	lastKeyPTS := map[uint32]int64{}
	havePTS := map[uint32]bool{}
	for {
		p, err := r.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			f, _ := model.AsFault(err)
			if f == nil {
				return idx, err
			}
			idx.addFault(f)
			idx.markCorrupt(len(idx.Packets))
			if !f.Recoverable {
				return idx, err
			}
			// Recoverable truncation: keep the clean prefix but still
			// surface the fault so callers can localize it explicitly.
			return idx, err
		}
		order := len(idx.Packets)
		idx.Packets = append(idx.Packets, p)
		idx.ByTrack[p.Track] = append(idx.ByTrack[p.Track], p)
		if !p.ChecksumOK {
			f := model.NewFault(model.FaultBadCRC, p.Track, p.Seq, p.PTSNS, p.EndPTSNS, true,
				"CRC32 mismatch; packet excluded from timeline")
			idx.addFault(f)
			idx.markCorrupt(order)
			if idx.BadCRCSeqs[p.Track] == nil {
				idx.BadCRCSeqs[p.Track] = map[uint64]bool{}
			}
			idx.BadCRCSeqs[p.Track][p.Seq] = true
			continue
		}
		idx.checkMonotonic(p, havePTS, lastPTS)
		idx.checkKeyframes(r.Tracks(), p, lastKeyPTS)
	}
	// A non-recoverable semantic fault (e.g. PTS regression) fails the build
	// even though every packet parsed; the index stays available for rollback.
	for _, f := range idx.Faults {
		if !f.Recoverable {
			return idx, f
		}
	}
	return idx, nil
}

func (idx *Index) addFault(f *model.Fault) {
	idx.Faults = append(idx.Faults, f)
	if idx.FirstFault == nil {
		idx.FirstFault = f // scan order equals packet order on the timeline
	}
}

func (idx *Index) markCorrupt(order int) {
	if idx.FirstCorruptGlobal < 0 || order < idx.FirstCorruptGlobal {
		idx.FirstCorruptGlobal = order
	}
}

func (idx *Index) checkMonotonic(p *Packet, have map[uint32]bool, last map[uint32]int64) {
	if have[p.Track] && p.PTSNS <= last[p.Track] {
		f := model.NewFault(model.FaultPTSRegression, p.Track, p.Seq, last[p.Track], p.PTSNS, false,
			fmt.Sprintf("PTS %d does not follow previous %d", p.PTSNS, last[p.Track]))
		idx.addFault(f)
		return
	}
	have[p.Track] = true
	last[p.Track] = p.PTSNS
}

func (idx *Index) checkKeyframes(tracks []TrackDesc, p *Packet, lastKey map[uint32]int64) {
	desc, ok := idx.desc(tracks, p.Track)
	if !ok || desc.Kind != KindVideo {
		return
	}
	if p.Keyframe {
		if first, ok := lastKey[p.Track]; ok && desc.MaxKeyIntervalNS > 0 && p.PTSNS-first > desc.MaxKeyIntervalNS {
			f := model.NewFault(model.FaultMissingKeyframe, p.Track, p.Seq, first, p.PTSNS, true,
				fmt.Sprintf("keyframe gap %dns exceeds max %dns", p.PTSNS-first, desc.MaxKeyIntervalNS))
			idx.addFault(f)
		}
		lastKey[p.Track] = p.PTSNS
		idx.Keyframes[p.Track] = append(idx.Keyframes[p.Track], KeyframeIndexEntry{Seq: p.Seq, PTSNS: p.PTSNS})
		return
	}
	if _, ok := lastKey[p.Track]; !ok {
		f := model.NewFault(model.FaultMissingKeyframe, p.Track, p.Seq, p.PTSNS, p.PTSNS, true,
			"video track begins without a keyframe")
		idx.addFault(f)
		lastKey[p.Track] = p.PTSNS
	}
}

func (idx *Index) desc(tracks []TrackDesc, id uint32) (TrackDesc, bool) {
	for _, t := range tracks {
		if t.ID == id {
			return t, true
		}
	}
	return TrackDesc{}, false
}

// ValidPackets returns the global clean prefix: all packets before the first
// corrupt one in stream order. TrackValid additionally enforces a per-track
// crop so a defect on one track never discards intact packets on another.
func (idx *Index) ValidPackets() []*Packet {
	if idx.FirstCorruptGlobal < 0 {
		return idx.Packets
	}
	return idx.Packets[:idx.FirstCorruptGlobal]
}

// FirstCorruptSeq returns the sequence number of the first bad-CRC packet on
// the given track, or -1 when that track has no checksum defect.
func (idx *Index) FirstCorruptSeq(track uint32) int64 {
	best := int64(-1)
	for seq := range idx.BadCRCSeqs[track] {
		if best == -1 || int64(seq) < best {
			best = int64(seq)
		}
	}
	return best
}

// TrackValid returns the clean packets belonging to one track: checksum-valid
// packets whose sequence precedes the track's own first corrupt packet. A
// defect on another track therefore never crops this track's output.
func (idx *Index) TrackValid(track uint32) []*Packet {
	cut := idx.FirstCorruptSeq(track)
	var out []*Packet
	for _, p := range idx.Packets {
		if p.Track != track || !p.ChecksumOK {
			continue
		}
		if cut >= 0 && int64(p.Seq) >= cut {
			continue
		}
		out = append(out, p)
	}
	return out
}
