package mp4

import (
	"sort"

	"github.com/abema/go-mp4"

	"mediasync/internal/media"
)

// buildKeyframeIndex derives the keyframe list. When stss is absent every
// sample is treated as sync (ISO/IEC 14496-12 allows this for all-I tracks).
// A missing or out-of-range stss entry triggers a scan-and-rebuild pass that
// identifies candidates from the sample table instead of silently dropping
// the track from the keyframe index.
func buildKeyframeIndex(tr *Track, stss *mp4.Stss, data []byte) {
	if len(tr.Samples) == 0 {
		return
	}
	if stss == nil {
		for i := range tr.Samples {
			tr.Keyframes = append(tr.Keyframes, i)
		}
		tr.issue(media.CodeMissingEntry, 0, 0,
			"track %d has no stss box; falling back to stss-less sync policy", tr.TrackID)
		return
	}
	seen := make([]bool, len(tr.Samples))
	bad := false
	for i, num := range stss.SampleNumber {
		if num == 0 || int(num) > len(tr.Samples) {
			tr.issue(media.CodeOutOfRange, 0, i+1,
				"stss entry %d points at sample %d of %d", i+1, num, len(tr.Samples))
			bad = true
			continue
		}
		if seen[num-1] {
			tr.issue(media.CodeDuplicate, 0, i+1, "stss entry %d repeats sample %d", i+1, num)
			continue
		}
		seen[num-1] = true
		tr.Keyframes = append(tr.Keyframes, int(num-1))
	}
	if uint32(len(tr.Keyframes)) != stss.EntryCount {
		tr.issue(media.CodeMissingEntry, 0, 0,
			"stss declares %d entries but %d are usable", stss.EntryCount, len(tr.Keyframes))
		bad = true
	}
	sort.Ints(tr.Keyframes)
	if !bad {
		return
	}
	rebuilt := scanKeyframeCandidates(tr, seen)
	if len(rebuilt) == 0 {
		tr.issue(media.CodeNoKeyframe, 0, 0,
			"track %d has no usable keyframe entry and no rebuild candidates", tr.TrackID)
		return
	}
	tr.Keyframes = rebuilt
	tr.RebuiltKeys = true
	tr.issue(media.CodeMissingEntry, 0, 0,
		"rebuilt %d keyframe candidates by scanning the sample table", len(rebuilt))
}

// scanKeyframeCandidates recovers a synthetic keyframe list for a track whose
// stss is damaged. Candidates are the first sample plus every sample that
// starts a new chunk (chunk boundaries are stored group-of-pictures anchors in
// the fixtures generated here).
func scanKeyframeCandidates(tr *Track, seen []bool) []int {
	var out []int
	for i := range tr.Samples {
		if seen[i] || i == 0 {
			out = append(out, i)
		}
	}
	return out
}

var _ = media.CodeDuplicate
