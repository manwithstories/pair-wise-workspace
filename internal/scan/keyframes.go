// Package scan consumes unified sample events and locates key-frame cadence
// and damage segments without guessing frame boundaries.
package scan

import (
	"avscan/internal/model"
)

// KeyFrameReport summarizes one video track's sync-sample cadence.
type KeyFrameReport struct {
	TrackID       uint32
	SampleCount   int
	SyncCount     int
	// GapNS holds every interval between consecutive sync samples in ns.
	GapNS         []int64
	MedianGapNS   int64
	MaxGapNS      int64
	// MissingSync is true when the video track has no stss table at all.
	MissingSync   bool
}

// AnalyzeKeyFrames derives inter-key-frame intervals from a video track. It
// never invents frame boundaries: without an stss table the track is flagged
// MissingSync instead of being treated as all-intra.
func AnalyzeKeyFrames(tr *model.Track) KeyFrameReport {
	r := KeyFrameReport{TrackID: tr.ID, SampleCount: len(tr.Samples)}
	if tr.Kind != model.KindVideo {
		return r
	}
	if tr.SyncSamples == nil {
		r.MissingSync = true
		return r
	}
	r.SyncCount = len(tr.SyncSamples)
	if len(tr.Samples) == 0 || len(tr.SyncSamples) < 2 {
		return r
	}
	// Map 1-based sync sample numbers onto nanosecond presentation starts.
	syncTimes := make([]int64, 0, len(tr.SyncSamples))
	for _, oneBased := range tr.SyncSamples {
		idx := int(oneBased) - 1
		if idx < 0 || idx >= len(tr.Samples) {
			// stss points past the sample table: a damage fault, not a gap.
			continue
		}
		syncTimes = append(syncTimes, tr.Samples[idx].StartNS)
	}
	for i := 1; i < len(syncTimes); i++ {
		gap := syncTimes[i] - syncTimes[i-1]
		if gap > 0 {
			r.GapNS = append(r.GapNS, gap)
		}
	}
	if med, ok := model.Median(r.GapNS); ok {
		r.MedianGapNS = med
	}
	for _, g := range r.GapNS {
		if g > r.MaxGapNS {
			r.MaxGapNS = g
		}
	}
	return r
}

// AnalyzeAllKeyFrames runs the analysis over every video track.
func AnalyzeAllKeyFrames(tl *model.Timeline) []KeyFrameReport {
	out := make([]KeyFrameReport, 0, len(tl.Tracks))
	for _, tr := range tl.Tracks {
		if tr.Kind == model.KindVideo {
			out = append(out, AnalyzeKeyFrames(tr))
		}
	}
	return out
}
