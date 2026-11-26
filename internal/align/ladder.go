package align

import (
	"sort"

	"avpipe/internal/index"
	"avpipe/internal/media"
)

// Boundary is one rendition's keyframe anchor for a common tactical slot.
type Boundary struct {
	Rendition string            `json:"rendition"`
	Bitrate   int64             `json:"bitrate"`
	Seq       int               `json:"seq"`
	Tick      uint64            `json:"tick_90k"`
	Epoch     int               `json:"epoch"`
	Offset    int               `json:"offset"`
	Present   bool              `json:"present"`
	Reasons   []media.ErrorCode `json:"reasons,omitempty"`
}

// Slot retains its sequence number even when one side has no usable boundary.
type Slot struct {
	Seq        int        `json:"seq"`
	CommonTick uint64     `json:"common_tick_90k"`
	Epoch      int        `json:"epoch"`
	Aligned    bool       `json:"aligned"`
	Gap        bool       `json:"gap"`
	Boundaries []Boundary `json:"boundaries"`
	Reasons    []string   `json:"reasons,omitempty"`
}

type Ladder struct {
	Slots        []*Slot `json:"slots"`
	SegmentCount int     `json:"segment_count"`
	AlignedSlots int     `json:"aligned_slots"`
	GapSlots     int     `json:"gap_slots"`
	Renditions   int     `json:"renditions"`
}

// Align joins at least two rendition indexes on their shared slot sequence.
// Ticks use media.TickHz. A missing or CC-tainted boundary never shifts later
// sequence numbers; the slot is emitted as an explicit GAP instead.
func Align(indexes []*index.Index, segments int) (*Ladder, error) {
	if len(indexes) < 2 {
		return nil, media.NewError(media.CodeConfig, -1, "alignment requires at least two bitrate renditions, got %d", len(indexes))
	}
	if segments <= 0 {
		return nil, media.NewError(media.CodeConfig, -1, "segments must be positive")
	}
	l := &Ladder{SegmentCount: segments, Renditions: len(indexes)}
	bySeq := make([]map[int]*index.Keyframe, len(indexes))
	affected := make([]map[int][]index.AffectedInterval, len(indexes))
	for ri, idx := range indexes {
		bySeq[ri] = map[int]*index.Keyframe{}
		for i := range idx.Keyframes {
			k := &idx.Keyframes[i]
			bySeq[ri][k.Seq] = k
		}
		affected[ri] = map[int][]index.AffectedInterval{}
		for _, a := range idx.AffectedIntervals {
			affected[ri][a.Seq] = append(affected[ri][a.Seq], a)
		}
	}

	for seq := 0; seq < segments; seq++ {
		slot := &Slot{Seq: seq, Aligned: true}
		var tickCandidates []uint64
		epoch := 0
		for ri, idx := range indexes {
			k := bySeq[ri][seq]
			b := Boundary{Rendition: idx.Rendition, Bitrate: idx.Bitrate, Seq: seq}
			for _, a := range affected[ri][seq] {
				b.Reasons = append(b.Reasons, a.Reason)
				slot.Reasons = append(slot.Reasons, string(a.Reason)+":"+idx.Rendition)
			}
			if k == nil {
				b.Present = false
				b.Reasons = append(b.Reasons, media.CodeMissingIDR)
				slot.Reasons = append(slot.Reasons, string(media.CodeMissingIDR)+":"+idx.Rendition)
				slot.Aligned = false
				slot.Gap = true
			} else {
				b.Present = true
				b.Tick = k.Tick
				b.Epoch = k.Epoch
				b.Offset = k.Offset
				tickCandidates = append(tickCandidates, k.Tick)
				epoch = k.Epoch
				if len(b.Reasons) > 0 {
					slot.Aligned = false
					slot.Gap = true
				}
			}
			slot.Boundaries = append(slot.Boundaries, b)
		}
		if len(tickCandidates) > 0 {
			sort.Slice(tickCandidates, func(i, j int) bool { return tickCandidates[i] < tickCandidates[j] })
			slot.CommonTick = tickCandidates[len(tickCandidates)/2]
			slot.Epoch = epoch
		} else {
			slot.Gap = true
			slot.Aligned = false
		}
		if slot.Aligned {
			l.AlignedSlots++
		}
		if slot.Gap {
			l.GapSlots++
		}
		l.Slots = append(l.Slots, slot)
	}
	return l, nil
}

// Usable returns slots with complete, untainted boundaries.
func (l *Ladder) Usable() []*Slot {
	var out []*Slot
	for _, s := range l.Slots {
		if s.Aligned {
			out = append(out, s)
		}
	}
	return out
}

// SequenceBoundaries returns the common tick boundaries in sequence order.
func (l *Ladder) SequenceBoundaries() []uint64 {
	out := make([]uint64, 0, len(l.Slots))
	for _, s := range l.Slots {
		out = append(out, s.CommonTick)
	}
	return out
}
