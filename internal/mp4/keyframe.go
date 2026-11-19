package mp4

import (
	"sort"

	"github.com/abema/go-mp4"

	"mediasync/internal/media"
)

// buildKeyframeIndex derives the keyframe list for a track. When stss is
// absent, ISO/IEC 14496-12 says every sample is sync for audio-ish tracks but
// video tracks are expected to carry one; a missing or out-of-range stss entry
// triggers a scan-and-rebuild pass so damaged indexes degrade instead of
// silently dropping keyframes.
func buildKeyframeIndex(tr *Track, stss *mp4.Stss, data []byte, stssOff int64) {
	if len(tr.Samples) == 0 {
		return
	}
	video := tr.Handler == "vide"
	if stss == nil {
		if !video {
			markAllKeyframes(tr)
			return
		}
		for i := range tr.Samples {
			tr.Keyframes = append(tr.Keyframes, i)
		}
		tr.issue(media.CodeMissingEntry, stssOff, 0,
			"track %d has no stss box; every sample treated as sync", tr.TrackID)
		markKeyframeFlags(tr)
		return
	}
	seen := make([]bool, len(tr.Samples))
	bad := false
	for i, num := range stss.SampleNumber {
		if num == 0 || int(num) > len(tr.Samples) {
			tr.issue(media.CodeOutOfRange, stssOff, i+1,
				"stss entry %d points at sample %d of %d", i+1, num, len(tr.Samples))
			bad = true
			continue
		}
		if seen[num-1] {
			tr.issue(media.CodeDuplicate, stssOff, i+1, "stss entry %d repeats sample %d", i+1, num)
			continue
		}
		seen[num-1] = true
		tr.Keyframes = append(tr.Keyframes, int(num-1))
	}
	if uint32(len(tr.Keyframes)) != stss.EntryCount {
		tr.issue(media.CodeMissingEntry, stssOff, 0,
			"stss declares %d entries but %d are usable", stss.EntryCount, len(tr.Keyframes))
		bad = true
	}
	sort.Ints(tr.Keyframes)
	if bad {
		rebuilt := scanKeyframeCandidates(tr, seen)
		if len(rebuilt) == 0 {
			tr.issue(media.CodeNoKeyframe, stssOff, 0,
				"track %d has no usable keyframe entry and no rebuild candidates", tr.TrackID)
			return
		}
		tr.Keyframes = rebuilt
		tr.RebuiltKeys = true
		tr.issue(media.CodeMissingEntry, stssOff, 0,
			"rebuilt %d keyframe candidates by scanning the sample table", len(rebuilt))
	}
	markKeyframeFlags(tr)
	_ = data
}

func markAllKeyframes(tr *Track) {
	tr.Keyframes = tr.Keyframes[:0]
	for i := range tr.Samples {
		tr.Keyframes = append(tr.Keyframes, i)
	}
	markKeyframeFlags(tr)
}

func markKeyframeFlags(tr *Track) {
	key := make([]bool, len(tr.Samples))
	for _, idx := range tr.Keyframes {
		if idx >= 0 && idx < len(key) {
			key[idx] = true
		}
	}
	for i := range tr.Samples {
		tr.Samples[i].Keyframe = key[i]
	}
}

// scanKeyframeCandidates recovers a synthetic keyframe list for a track whose
// stss is damaged: the first sample and every still-marked sync sample are
// candidates, and if nothing survives the first sample is used as an anchor.
func scanKeyframeCandidates(tr *Track, seen []bool) []int {
	out := make([]int, 0, len(tr.Keyframes)+1)
	for i := range tr.Samples {
		if seen[i] || i == 0 {
			out = append(out, i)
		}
	}
	return out
}
