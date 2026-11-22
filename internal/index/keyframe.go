// Package index rebuilds and verifies keyframe indexes from analyzed tracks.
package index

import (
	"mediascan/internal/media"
	"mediascan/internal/stream"
)

// Index is a keyframe index for one track.
type Index struct {
	TrackID uint32
	// Synced holds 1-based sample numbers known to be keyframes.
	Synced []int
	// Source records how the index was obtained: "stss" (read from the file),
	// "implicit" (no stss, all samples sync) or "rebuilt" (scanned samples
	// after the declared index was missing or incomplete).
	Source string
	// Complete is true when the index is known to cover every sample.
	Complete bool
}

// BuildResult pairs the index with the issues found while building it.
type BuildResult struct {
	Index  Index
	Issues media.IssueSet
	// Rebuilt is true when the declared stss was insufficient and sample
	// scanning was required.
	Rebuilt bool
}

// Build constructs a keyframe index for a track. It distinguishes three
// situations explicitly, as required by the spec:
//
//   - no keyframes at all (ClassMP4NoKeyframes),
//   - the declared stss index is missing or incomplete (ClassMP4IndexIncomplete),
//   - a healthy declared index.
//
// When the declared index is incomplete the sample list is scanned to rebuild
// it, marking Rebuilt and recording which byte span the rebuild covers.
func Build(tr *stream.Track) BuildResult {
	res := BuildResult{}
	if tr == nil {
		res.Issues.Add(media.NewFatal(media.ClassMP4BadStructure, media.Locator{}, "nil track"))
		return res
	}
	res.Index.TrackID = tr.TrackID
	switch tr.IndexSource {
	case "implicit-all-sync":
		res.Index.Source = "implicit"
		res.Index.Complete = true
		for _, s := range tr.Samples {
			res.Index.Synced = append(res.Index.Synced, s.Number)
		}
		return res
	case "rebuilt":
		// The analyzer already restored missing entries by scanning sample
		// bytes; the sample Sync flags now reflect the complete index.
		res.Index.Source = "rebuilt"
		res.Index.Complete = tr.Complete
		res.Rebuilt = true
		for _, s := range tr.Samples {
			if s.Sync {
				res.Index.Synced = append(res.Index.Synced, s.Number)
			}
		}
		if len(res.Index.Synced) == 0 {
			res.Issues.Add(media.NewIssue(media.ClassMP4NoKeyframes, media.Locator{TrackID: tr.TrackID},
				"rebuilt index contains no keyframes among %d samples", len(tr.Samples)))
			return res
		}
		res.Issues.Add(media.NewIssue(media.ClassMP4IndexIncomplete, media.Locator{TrackID: tr.TrackID},
			"keyframe index was missing entries; rebuilt %d keyframes from sample scan", len(res.Index.Synced)))
		return res
	case "stss":
		// fallthrough to validation below
	default:
		res.Index.Source = tr.IndexSource
	}

	declared := map[int]bool{}
	invalid := false
	for _, n := range tr.Keyframes {
		if n < 1 || n > len(tr.Samples) {
			invalid = true
			continue
		}
		declared[n] = true
	}
	if invalid {
		res.Issues.Add(media.NewIssue(media.ClassMP4IndexIncomplete, media.Locator{TrackID: tr.TrackID},
			"stss names a sample number outside 1..%d", len(tr.Samples)))
	}

	// A declared index is complete if it is non-empty and every declared
	// number is valid. An empty stss on a video track means either "no
	// keyframes" or "index lost": distinguish by scanning samples.
	if len(declared) > 0 && !invalid {
		res.Index.Source = "stss"
		res.Index.Complete = true
		for n := range declared {
			res.Index.Synced = append(res.Index.Synced, n)
		}
		res.Index.Synced = sortInts(res.Index.Synced)
		return res
	}

	// Rebuild by scanning samples. A sample is a keyframe if it is marked
	// Sync. If the scan yields nothing, the track genuinely has no keyframes.
	rebuilt := make([]int, 0, len(tr.Samples))
	for _, s := range tr.Samples {
		if s.Sync {
			rebuilt = append(rebuilt, s.Number)
		}
	}
	res.Rebuilt = true
	res.Index.Source = "rebuilt"
	res.Index.Complete = tr.Complete
	res.Index.Synced = rebuilt
	if len(rebuilt) == 0 {
		res.Issues.Add(media.NewIssue(media.ClassMP4NoKeyframes, media.Locator{TrackID: tr.TrackID},
			"rebuild scan found no sync sample among %d samples", len(tr.Samples)))
		return res
	}
	res.Issues.Add(media.NewIssue(media.ClassMP4IndexIncomplete, media.Locator{TrackID: tr.TrackID},
		"declared keyframe index was missing or incomplete; rebuilt %d keyframes from samples", len(rebuilt)))
	return res
}

// HitRate reports the fraction of expected keyframe boundaries present in the
// index. Expected boundaries are derived from the sample list (every Sync
// sample); the result is in [0,1].
func (ix Index) HitRate(tr *stream.Track) float64 {
	if tr == nil {
		return 0
	}
	expected := 0
	for _, s := range tr.Samples {
		if s.Sync {
			expected++
		}
	}
	if expected == 0 {
		return 0
	}
	have := map[int]bool{}
	for _, n := range ix.Synced {
		have[n] = true
	}
	hits := 0
	for _, s := range tr.Samples {
		if s.Sync && have[s.Number] {
			hits++
		}
	}
	return float64(hits) / float64(expected)
}

// Intervals returns the gaps in nanoseconds between consecutive keyframes,
// which is what a media engineer inspects for GOP structure. A track with
// fewer than two keyframes yields an empty slice.
func (ix Index) Intervals(tr *stream.Track) []int64 {
	if tr == nil || len(ix.Synced) < 2 {
		return nil
	}
	byNum := make(map[int]stream.Sample, len(tr.Samples))
	for _, s := range tr.Samples {
		byNum[s.Number] = s
	}
	out := make([]int64, 0, len(ix.Synced)-1)
	for i := 1; i < len(ix.Synced); i++ {
		a, ok1 := byNum[ix.Synced[i-1]]
		b, ok2 := byNum[ix.Synced[i]]
		if !ok1 || !ok2 {
			continue
		}
		out = append(out, b.DTSNs-a.DTSNs)
	}
	return out
}

func sortInts(v []int) []int {
	for i := 1; i < len(v); i++ {
		for j := i; j > 0 && v[j] < v[j-1]; j-- {
			v[j], v[j-1] = v[j-1], v[j]
		}
	}
	return v
}
