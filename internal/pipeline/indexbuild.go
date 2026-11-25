package pipeline

import (
	"fmt"
	"os"

	"archivectl/internal/index"
	"archivectl/internal/layout"
)

// BuildIndex analyzes the group and assembles the canonical index,
// hashing the already-written output chunks. It never writes files, so
// the auditor can call it repeatedly to prove repeatability.
func BuildIndex(root, group string) (*index.Index, *Analysis, error) {
	a, err := Analyze(root, group)
	if err != nil {
		return nil, nil, err
	}
	idx := &index.Index{
		Version:           index.IndexVersion,
		InputSHA256:       a.InputDigest,
		CommonTimescale:   720000,
		MetadataConflicts: a.Conflicts,
	}

	for _, track := range layout.Tracks {
		tl := a.Timelines[track]
		for _, r := range tl.Ranges {
			sourceRel := layout.SourceRel(track, r.SegmentIndex)
			outputRel := layout.OutputRel(track, r.SegmentIndex)
			outPath := layout.OutputPath(root, group, track, r.SegmentIndex)
			st, err := os.Stat(outPath)
			if err != nil {
				return nil, nil, fmt.Errorf("pipeline: output chunk missing for index: %w", err)
			}
			sha, err := index.SHA256File(outPath)
			if err != nil {
				return nil, nil, err
			}
			idx.Chunks = append(idx.Chunks, index.ChunkForRange(r, tl, sourceRel, outputRel, st.Size(), sha))
			idx.Breakpoints = append(idx.Breakpoints, index.BreakpointsFromTimeline(tl)...)
		}
	}

	if a.RTP != nil {
		idx.RTP = append(idx.RTP, index.RTPSummary{
			Stream: "audio-rtp", TotalLost: a.RTP.TotalLost, Recovered: a.RTP.Recovered,
			Duplicates: a.RTP.Duplicates, Reordered: a.RTP.Reordered,
			UnrecoveredGaps: len(a.RTP.UnrecoveredGaps), LossRecovery: a.RTP.LossRecovery,
		})
		idx.UnrecoveredGaps = len(a.RTP.UnrecoveredGaps)
	}
	return idx, a, nil
}
