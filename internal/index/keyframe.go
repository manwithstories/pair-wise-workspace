package index

import (
	"avpipe/internal/media"
	"avpipe/internal/ts"
)

// Keyframe is one decoded AVC IDR anchor on a representation timeline.
type Keyframe struct {
	Index       int    `json:"index"`
	Rendition   string `json:"rendition"`
	PID         uint16 `json:"pid"`
	Offset      int    `json:"offset"`
	PacketIndex int    `json:"packet_index"`
	Tick        uint64 `json:"tick_90k"`
	Epoch       int    `json:"epoch"`
	Seq         int    `json:"seq"`
}

// AudioStamp is a decoded PES PTS for an audio access unit.
type AudioStamp struct {
	PID    uint16 `json:"pid"`
	Offset int    `json:"offset"`
	Tick   uint64 `json:"tick_90k"`
	Epoch  int    `json:"epoch"`
}

// AffectedInterval records a timeline slot that must take the alignment error
// path instead of being silently treated as a clean boundary.
type AffectedInterval struct {
	Seq    int             `json:"seq"`
	Reason media.ErrorCode `json:"reason"`
	Detail string          `json:"detail"`
	Offset int             `json:"offset"`
}

// Index is the deterministic keyframe/audio/CC result for one rendition.
type Index struct {
	Rendition         string             `json:"rendition"`
	Bitrate           int64              `json:"bitrate"`
	Keyframes         []Keyframe         `json:"keyframes"`
	Audio             []AudioStamp       `json:"audio"`
	CCGaps            []ts.CCGap         `json:"cc_gaps"`
	AffectedIntervals []AffectedInterval `json:"affected_intervals"`
	Packets           int                `json:"packets"`
	ExpectedSlots     int                `json:"expected_slots"`
}

type BuildConfig struct {
	Rendition     string
	Bitrate       int64
	VideoPID      uint16
	AudioPID      uint16
	ExpectedSlots int
	// PacketsPerSlot maps a CC gap packet to its tactical interval.
	PacketsPerSlot int
}

// Build scans already-framed packets. It never invents an IDR: a slot without
// an AVC NAL type 5 simply has no anchor and is reported by the caller's
// expected-slot reconciliation.
func Build(packets []ts.Packet, cfg BuildConfig) (*Index, error) {
	idx := &Index{Rendition: cfg.Rendition, Bitrate: cfg.Bitrate, ExpectedSlots: cfg.ExpectedSlots, Packets: len(packets)}
	cc := ts.NewCCTracker()
	lastPTS := map[uint16]uint64{}
	havePTS := map[uint16]bool{}

	for _, p := range packets {
		if gap := cc.Check(p); gap != nil {
			idx.CCGaps = append(idx.CCGaps, *gap)
			seq := slotForOffset(p.Index, cfg.PacketsPerSlot)
			idx.addAffected(AffectedInterval{
				Seq: seq, Reason: media.CodeCCGap, Offset: gap.Offset,
				Detail: "continuity counter jumped",
			})
		}
		if !p.HasPayload {
			continue
		}
		if p.PID == cfg.VideoPID && p.PUSI {
			pts, pesPayload, ok := ParsePES(p.Payload)
			tick := pts
			epoch := 0
			if ok {
				tick, epoch = expandPIDPTS(lastPTS, havePTS, p.PID, pts)
			}
			if NALContainsIDR(pesPayload) {
				idx.Keyframes = append(idx.Keyframes, Keyframe{
					Index: len(idx.Keyframes), Rendition: cfg.Rendition, PID: p.PID,
					Offset: p.Offset, PacketIndex: p.Index, Tick: tick, Epoch: epoch,
					Seq: slotForOffset(p.Index, cfg.PacketsPerSlot),
				})
			}
		}
		if p.PID == cfg.AudioPID && p.PUSI {
			if pts, _, ok := ParsePES(p.Payload); ok {
				tick, epoch := expandPIDPTS(lastPTS, havePTS, p.PID, pts)
				idx.Audio = append(idx.Audio, AudioStamp{
					PID: p.PID, Offset: p.Offset, Tick: tick, Epoch: epoch,
				})
			}
		}
	}
	return idx, nil
}

func expandPIDPTS(last map[uint16]uint64, have map[uint16]bool, pid uint16, raw uint64) (uint64, int) {
	if !have[pid] {
		have[pid] = true
		last[pid] = raw
		return raw, 0
	}
	prev := last[pid]
	var delta uint64
	if raw >= prev {
		delta = raw - prev
	} else {
		delta = raw + media.PTSMod90k - prev
	}
	expanded := prev + delta
	last[pid] = expanded
	// Fixture timelines do not encode a PTS discontinuity epoch in PES; PCR
	// epochs remain the authority. Keep epoch zero unless clock joins later.
	return expanded, 0
}

func slotForOffset(packetIndex, perSlot int) int {
	if perSlot <= 0 {
		return 0
	}
	return packetIndex / perSlot
}

func (idx *Index) addAffected(a AffectedInterval) {
	for i := range idx.AffectedIntervals {
		if idx.AffectedIntervals[i].Seq == a.Seq && idx.AffectedIntervals[i].Reason == a.Reason {
			return
		}
	}
	idx.AffectedIntervals = append(idx.AffectedIntervals, a)
}

// ExpectedHits returns found/expected keyframe counts across supplied indexes.
func ExpectedHits(indexes []*Index) (found, expected int) {
	for _, idx := range indexes {
		found += len(idx.Keyframes)
		expected += idx.ExpectedSlots
	}
	return found, expected
}

// MarkMissingSlots compares IDR sequence anchors with the deterministic slot
// plan and marks absent boundaries.
func (idx *Index) MarkMissingSlots() []int {
	seen := map[int]bool{}
	for _, k := range idx.Keyframes {
		seen[k.Seq] = true
	}
	var missing []int
	for s := 0; s < idx.ExpectedSlots; s++ {
		if !seen[s] {
			missing = append(missing, s)
			idx.addAffected(AffectedInterval{
				Seq: s, Reason: media.CodeMissingIDR,
				Detail: "expected AVC IDR anchor absent", Offset: -1,
			})
		}
	}
	return missing
}

// ParsePES extracts PTS and PES payload. It supports the MPEG-2 PES header
// form used by the deterministic fixtures.
func ParsePES(payload []byte) (pts uint64, mediaPayload []byte, ok bool) {
	if len(payload) < 9 || payload[0] != 0 || payload[1] != 0 || payload[2] != 1 {
		return 0, payload, false
	}
	headerDataLen := int(payload[8])
	start := 9 + headerDataLen
	if start > len(payload) {
		return 0, nil, false
	}
	if len(payload) >= 14 && payload[7]&0x80 != 0 && headerDataLen >= 5 {
		pts = DecodePTS(payload[9:14])
		return pts, payload[start:], true
	}
	return 0, payload[start:], true
}

func DecodePTS(b []byte) uint64 {
	if len(b) < 5 {
		return 0
	}
	return uint64(b[0]&0x0e)<<29 | uint64(b[1])<<22 | uint64(b[2]&0xfe)<<14 |
		uint64(b[3])<<7 | uint64(b[4])>>1
}

// NALContainsIDR scans Annex B start codes and returns true for nal_unit_type 5.
func NALContainsIDR(b []byte) bool {
	for i := 0; i+3 < len(b); i++ {
		if b[i] == 0 && b[i+1] == 0 && b[i+2] == 1 {
			nal := i + 3
			if nal < len(b) && b[nal]&0x1f == 5 {
				return true
			}
		}
		if i+4 < len(b) && b[i] == 0 && b[i+1] == 0 && b[i+2] == 0 && b[i+3] == 1 {
			nal := i + 4
			if nal < len(b) && b[nal]&0x1f == 5 {
				return true
			}
		}
	}
	return false
}
