package segment

import (
	"sort"
)

// OverlapToleranceMS is how much a segment may run past the next segment's
// source timestamp before the pair is classified as an overlap.
const OverlapToleranceMS = 100

// GapInterval describes one run of consecutive missing sequence numbers.
type GapInterval struct {
	Rendition int   `json:"rendition"`
	FromSeq   int64 `json:"from_seq"`
	ToSeq     int64 `json:"to_seq"`
	StartMS   int64 `json:"start_ms"`
	EndMS     int64 `json:"end_ms"`
	Count     int64 `json:"count"`
}

// RenditionIndex is the reconstructed timeline of one bitrate ladder rung.
type RenditionIndex struct {
	Rendition  int               `json:"rendition"`
	Profile    Profile           `json:"profile"`
	Slots      map[int64]Segment `json:"-"`
	MinSeq     int64             `json:"min_seq"`
	MaxSeq     int64             `json:"max_seq"`
	SlotMS     int64             `json:"slot_ms"`
	BaseTS     int64             `json:"base_ts_ms"`
	Gaps       []GapInterval     `json:"gaps"`
	MaxDriftMS int64             `json:"max_drift_ms"`
	Overlaps   []Failure         `json:"overlaps"`
	ZeroByte   []Segment         `json:"zero_byte"`
}

// Index holds the four per-rendition timelines rebuilt from a directory scan.
type Index struct {
	Renditions map[int]*RenditionIndex
	Ordered    []*RenditionIndex
	SlotMS     int64
	BaseTS     int64
	Failures   []Failure
}

// BuildIndex groups scanned segments by rendition and rebuilds each timeline.
// slotMS is the encoder's nominal segment duration (EXT-X-TARGETDURATION*1000).
// The sequence space is assumed to start at zero and run up to MaxSeq.
func BuildIndex(segments []Segment, slotMS int64) *Index {
	index := &Index{
		Renditions: make(map[int]*RenditionIndex),
		SlotMS:     slotMS,
	}
	baseTS := int64(-1)
	for _, seg := range segments {
		if baseTS < 0 || seg.SourceTS < baseTS {
			baseTS = seg.SourceTS
		}
	}
	if baseTS < 0 {
		baseTS = 0
	}
	index.BaseTS = baseTS

	for _, profile := range Profiles {
		rIndex := &RenditionIndex{
			Rendition: profile.Index,
			Profile:   profile,
			Slots:     make(map[int64]Segment),
			SlotMS:    slotMS,
			BaseTS:    baseTS,
		}
		index.Renditions[profile.Index] = rIndex
		index.Ordered = append(index.Ordered, rIndex)
	}

	for _, seg := range segments {
		rIndex := index.Renditions[seg.Rendition]
		rIndex.Slots[seg.Seq] = seg
		if seg.Size == 0 {
			rIndex.ZeroByte = append(rIndex.ZeroByte, seg)
		}
		drift := seg.MeasuredMS - seg.DeclaredMS
		if drift < 0 {
			drift = -drift
		}
		if drift > rIndex.MaxDriftMS {
			rIndex.MaxDriftMS = drift
		}
	}

	for _, rIndex := range index.Ordered {
		finalizeRendition(rIndex)
		index.Failures = append(index.Failures, rIndex.Overlaps...)
		for _, seg := range rIndex.ZeroByte {
			index.Failures = append(index.Failures, Failure{
				Kind:      "zero_byte",
				File:      seg.FileName,
				Detail:    "segment file is empty",
				Rendition: rIndex.Rendition,
				Seq:       seg.Seq,
			})
		}
	}
	return index
}

func finalizeRendition(rIndex *RenditionIndex) {
	if len(rIndex.Slots) == 0 {
		rIndex.MinSeq, rIndex.MaxSeq = 0, -1
		return
	}
	seqs := make([]int64, 0, len(rIndex.Slots))
	for seq := range rIndex.Slots {
		seqs = append(seqs, seq)
	}
	sort.Slice(seqs, func(i, j int) bool { return seqs[i] < seqs[j] })
	rIndex.MinSeq, rIndex.MaxSeq = 0, seqs[len(seqs)-1]

	var gapFrom int64 = -1
	for seq := rIndex.MinSeq; seq <= rIndex.MaxSeq; seq++ {
		if _, ok := rIndex.Slots[seq]; ok {
			if gapFrom >= 0 {
				rIndex.addGap(gapFrom, seq-1)
				gapFrom = -1
			}
			continue
		}
		if gapFrom < 0 {
			gapFrom = seq
		}
	}
	if gapFrom >= 0 {
		rIndex.addGap(gapFrom, rIndex.MaxSeq)
	}

	for position := 1; position < len(seqs); position++ {
		current := rIndex.Slots[seqs[position-1]]
		next := rIndex.Slots[seqs[position]]
		endMS := current.SourceTS + current.MeasuredMS
		if endMS > next.SourceTS+OverlapToleranceMS {
			rIndex.Overlaps = append(rIndex.Overlaps, Failure{
				Kind:      "overlap",
				File:      current.FileName,
				Detail:    "segment runs past next segment source timestamp",
				Rendition: rIndex.Rendition,
				Seq:       current.Seq,
			})
		}
	}
}

func (rIndex *RenditionIndex) addGap(from, to int64) {
	rIndex.Gaps = append(rIndex.Gaps, GapInterval{
		Rendition: rIndex.Rendition,
		FromSeq:   from,
		ToSeq:     to,
		StartMS:   rIndex.BaseTS + from*rIndex.SlotMS,
		EndMS:     rIndex.BaseTS + (to+1)*rIndex.SlotMS,
		Count:     to - from + 1,
	})
}

// SortedSeqs returns present sequence numbers in ascending order.
func (rIndex *RenditionIndex) SortedSeqs() []int64 {
	seqs := make([]int64, 0, len(rIndex.Slots))
	for seq := range rIndex.Slots {
		seqs = append(seqs, seq)
	}
	sort.Slice(seqs, func(i, j int) bool { return seqs[i] < seqs[j] })
	return seqs
}

// GapCount returns the total number of missing sequence slots on a rendition.
func (rIndex *RenditionIndex) GapCount() int64 {
	var total int64
	for _, gap := range rIndex.Gaps {
		total += gap.Count
	}
	return total
}

// GapIntervalsCount returns how many contiguous gap runs this rendition has.
func (rIndex *RenditionIndex) GapIntervalsCount() int {
	return len(rIndex.Gaps)
}
