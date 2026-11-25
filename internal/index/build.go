package index

import (
	"fmt"

	"archivectl/internal/repair"
)

// Boundaries projects the normalized samples of one segment into index
// sample boundaries. Byte offsets are unchanged by the same-width tfdt
// patch, so they address both source and output chunk layouts.
func Boundaries(tt *repair.TrackTimeline, segmentIndex int) []SampleBoundary {
	var out []SampleBoundary
	for _, s := range tt.Samples {
		if s.SegmentIndex != segmentIndex {
			continue
		}
		out = append(out, SampleBoundary{
			SampleIndex: s.SampleIndex,
			DTSCommon:   s.DTS,
			PTSCommon:   s.PTS,
			Duration:    s.Duration,
			Size:        s.Size,
			Sync:        s.Sync,
			ByteOffset:  s.PayloadOffset,
		})
	}
	return out
}

// ChunkForRange builds the index chunk for one repaired segment range.
func ChunkForRange(r repair.SegmentRange, tt *repair.TrackTimeline, sourceRel, outputRel string, outSize int64, outSHA string) Chunk {
	return Chunk{
		ID:                fmt.Sprintf("%s-%s", tt.TrackID, SegmentToken(r.SegmentIndex)),
		TrackID:           tt.TrackID,
		SegmentIndex:      r.SegmentIndex,
		SourceRel:         sourceRel,
		OutputRel:         outputRel,
		Timescale:         r.NativeTimescale,
		CorrectedBase:     r.CorrectedNativeBase,
		StartCommon:       r.StartCommon,
		EndCommon:         r.EndCommon,
		OutputSize:        outSize,
		OutputSHA256:      outSHA,
		SampleCount:       r.SampleCount,
		SyncSampleIndexes: append([]int(nil), r.SyncSampleIndexes...),
		Samples:           Boundaries(tt, r.SegmentIndex),
	}
}

// BreakpointsFromTimeline projects timeline breakpoints into the index.
func BreakpointsFromTimeline(tt *repair.TrackTimeline) []Breakpoint {
	out := make([]Breakpoint, 0, len(tt.Breakpoints))
	for _, b := range tt.Breakpoints {
		out = append(out, Breakpoint{TrackID: tt.TrackID, SegmentIndex: b.SegmentIndex, Kind: b.Kind})
	}
	return out
}

// SegmentToken renders the stable zero-padded segment token for ids.
func SegmentToken(index int) string { return fmt.Sprintf("seg_%03d", index) }
