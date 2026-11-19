package scan

import (
	"fmt"

	"aircheck/internal/clock"
	"aircheck/internal/fault"
)

// Packet is one media sample located in the source file.
type Packet struct {
	TrackID  uint32
	Index    uint32 // 1-based sample number within the track
	Offset   int64  // raw byte offset of the sample payload
	Size     uint32
	DTS      int64 // unified 90 kHz base
	PTS      int64 // unified 90 kHz base
	Keyframe bool
}

// GapReport describes a keyframe interval that exceeded the threshold.
type GapReport struct {
	TrackID     uint32
	StartOffset int64 // byte offset of the keyframe starting the gap
	StartTime   int64 // unified base
	Length      int   // samples in the gap
}

// BuildPackets expands a track's sample tables into located packets with
// unified-base timestamps. Table inconsistencies are reported as faults;
// samples whose tables are missing are recoverable (missing index), samples
// pointing outside the file are not.
func BuildPackets(t *TrackInfo, fileSize int64) ([]Packet, fault.List) {
	var faults fault.List
	n := len(t.SampleSizes)
	if n == 0 {
		faults = append(faults, fault.New(fault.KindMissingIndex, fault.StageScan,
			t.ID, 0, -1, true, "track has no sample size table"))
		return nil, faults
	}

	// DTS per sample from stts.
	dts := make([]int64, 0, n)
	var cur int64
	for _, e := range t.Stts {
		for i := uint32(0); i < e.Count && len(dts) < n; i++ {
			dts = append(dts, cur)
			cur += int64(e.Delta)
		}
	}
	if len(dts) < n {
		faults = append(faults, fault.New(fault.KindMissingIndex, fault.StageScan,
			t.ID, 0, -1, true,
			fmt.Sprintf("stts covers %d of %d samples", len(dts), n)))
		for len(dts) < n {
			dts = append(dts, cur)
		}
	}

	// Byte offset per sample from stsc + stco.
	offsets, ok := sampleOffsets(t, fileSize, &faults)
	if !ok {
		return nil, faults
	}

	ts := &clock.TrackState{TrackID: t.ID, Timescale: t.Timescale}
	pkts := make([]Packet, 0, n)
	for i := 0; i < n; i++ {
		u, err := ts.Observe(dts[i], int64(offsets[i]))
		if err != nil {
			continue // overflow fault already recorded; skip sample
		}
		key := t.SyncSamples == nil || t.SyncSamples[uint32(i+1)]
		pkts = append(pkts, Packet{
			TrackID:  t.ID,
			Index:    uint32(i + 1),
			Offset:   int64(offsets[i]),
			Size:     t.SampleSizes[i],
			DTS:      u,
			PTS:      u, // no B-frame reordering in this tool's scope
			Keyframe: key,
		})
	}
	faults = append(faults, ts.Faults...)
	return pkts, faults
}

// sampleOffsets computes the file offset of every sample.
func sampleOffsets(t *TrackInfo, fileSize int64, faults *fault.List) ([]uint64, bool) {
	n := len(t.SampleSizes)
	offsets := make([]uint64, 0, n)
	if len(t.ChunkOffsets) == 0 || len(t.Stsc) == 0 {
		*faults = append(*faults, fault.New(fault.KindMissingIndex, fault.StageScan,
			t.ID, 0, -1, true, "missing chunk offset or sample-to-chunk table"))
		return nil, false
	}
	sample := 0
	for ci, chunkOff := range t.ChunkOffsets {
		// Samples per chunk for this chunk from the stsc runs.
		spc := t.Stsc[len(t.Stsc)-1].SamplesPerChunk
		for _, e := range t.Stsc {
			if uint32(ci+1) >= e.FirstChunk {
				spc = e.SamplesPerChunk
			} else {
				break
			}
		}
		off := chunkOff
		for s := uint32(0); s < spc && sample < n; s++ {
			end := off + uint64(t.SampleSizes[sample])
			if end > uint64(fileSize) {
				*faults = append(*faults, fault.New(fault.KindTruncated, fault.StageScan,
					t.ID, 0, int64(off), true,
					fmt.Sprintf("sample %d [%d,%d) beyond file size %d", sample+1, off, end, fileSize)))
				return offsets, false
			}
			offsets = append(offsets, off)
			off = end
			sample++
		}
	}
	if sample < n {
		*faults = append(*faults, fault.New(fault.KindMissingIndex, fault.StageScan,
			t.ID, 0, -1, true,
			fmt.Sprintf("chunk table covers %d of %d samples", sample, n)))
		return nil, false
	}
	return offsets, true
}

// KeyframeGaps returns every run of non-keyframe packets longer than maxGap
// samples, with the byte offset of the keyframe that opened the run.
func KeyframeGaps(pkts []Packet, maxGap int) []GapReport {
	if maxGap <= 0 {
		return nil
	}
	var out []GapReport
	gap := 0
	var startOff, startTime int64
	for _, p := range pkts {
		if p.Keyframe {
			if gap > maxGap {
				out = append(out, GapReport{TrackID: p.TrackID, StartOffset: startOff, StartTime: startTime, Length: gap})
			}
			gap = 0
			startOff, startTime = p.Offset, p.PTS
			continue
		}
		gap++
	}
	if gap > maxGap && len(pkts) > 0 {
		out = append(out, GapReport{TrackID: pkts[0].TrackID, StartOffset: startOff, StartTime: startTime, Length: gap})
	}
	return out
}
