package pipeline

import (
	"archivecheck.local/archivecheck/internal/demux"
	"archivecheck.local/archivecheck/internal/index"
)

// buildIndex derives the keyframe index from the blocks that were actually
// decoded. Cues points are attached only when they name an existing Cluster
// and tick, so a corrupt or absent Cues element degrades to a rebuilt index
// instead of producing phantom hits.
func buildIndex(scan *demux.Result) index.Index {
	samples := make([]index.Sample, len(scan.Samples))
	for i, s := range scan.Samples {
		samples[i] = index.Sample{
			TrackNumber:   s.TrackNumber,
			Tick:          s.Tick,
			Keyframe:      s.Keyframe,
			SourceOffset:  s.SourceOffset,
			ClusterOffset: s.ClusterOffset,
			FrameIndex:    s.FrameIndex,
		}
	}
	ix := index.FromSamples(samples)
	if len(scan.CuePoints) == 0 {
		return ix
	}
	ix.AttachCues(scan.CuePoints, scan.Info.SegmentDataOffset, scan.ClusterOffsets)
	return ix
}
