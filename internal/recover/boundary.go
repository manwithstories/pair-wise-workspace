package recover

import (
	"archivearchivist/internal/mp4"
)

// findRanges groups adjacent corrupt samples and searches outward for the
// decodable keyframes that bracket each run.
func findRanges(trackID string, sc *mp4.Scan, cfg Config) []Range {
	var out []Range
	if len(sc.Tracks) == 0 {
		return out
	}
	tr := sc.Tracks[0]
	i := 0
	for i < len(tr.Samples) {
		if !tr.Samples[i].Corrupt {
			i++
			continue
		}
		j := i
		for j+1 < len(tr.Samples) && tr.Samples[j+1].Corrupt {
			j++
		}
		out = append(out, bracket(trackID, tr, i, j, cfg))
		i = j + 1
	}
	return out
}

func bracket(trackID string, tr mp4.Track, i, j int, cfg Config) Range {
	r := Range{
		Track:          trackID,
		CorruptStart:   tr.Samples[i].Offset,
		CorruptEnd:     tr.Samples[j].Offset + int64(tr.Samples[j].Size) - 1,
		BeforeKeyframe: -1, AfterKeyframe: -1,
		RecoverableStart: -1, RecoverableEnd: -1,
		Status: "unrecoverable",
	}
	before, after := -1, -1
	lo := i - cfg.KeyframeSearch
	if lo < 0 {
		lo = 0
	}
	for k := i - 1; k >= lo; k-- {
		if tr.Samples[k].Keyframe && !tr.Samples[k].Corrupt {
			before = k
			break
		}
	}
	hi := j + cfg.KeyframeSearch
	if hi >= len(tr.Samples) {
		hi = len(tr.Samples) - 1
	}
	for k := j + 1; k <= hi; k++ {
		if tr.Samples[k].Keyframe && !tr.Samples[k].Corrupt {
			after = k
			break
		}
	}
	if before >= 0 {
		r.BeforeKeyframe = tr.Samples[before].Offset
	}
	if after >= 0 {
		r.AfterKeyframe = tr.Samples[after].Offset
	}
	if before < 0 && after < 0 {
		r.Detail = "no decodable keyframe before or after the damaged run"
		return r
	}
	if before >= 0 && after < 0 && j == len(tr.Samples)-1 && i == 0 {
		r.Detail = "the only decodable keyframe is the damaged sample itself"
		return r
	}
	// Decoding can restart from the preceding keyframe; without one, only the
	// frame after the damage is guaranteed, so the recoverable interval starts
	// at the first sample after the run.
	start := tr.Samples[j].Offset + int64(tr.Samples[j].Size)
	if before >= 0 {
		start = tr.Samples[before].Offset
	}
	end := tr.Samples[j].Offset
	if after >= 0 {
		end = tr.Samples[after].Offset + int64(tr.Samples[after].Size) - 1
	}
	if end < start {
		end = r.CorruptEnd
	}
	r.RecoverableStart, r.RecoverableEnd = start, end
	r.ErrorBoundUS = errorBound(tr, before, after, i, j)
	r.Status = "recovered"
	if before < 0 {
		r.Detail = "damage begins before the first keyframe; lead-in is not recoverable"
	}
	if after < 0 {
		r.Detail = "no keyframe after the damage; trailing samples are dropped"
	}
	if before >= 0 && after >= 0 {
		r.Detail = "bracketed by decodable keyframes"
	}
	return r
}

// errorBound is the uncertainty in where inside the recovered interval a cut
// can safely be placed, in microseconds: the span from the last good keyframe
// to the damage plus the damage to the next keyframe.
func errorBound(tr mp4.Track, before, after, i, j int) int64 {
	var ticks int64
	if before >= 0 {
		ticks += tr.Samples[i].DTS - tr.Samples[before].DTS
	}
	if after >= 0 {
		ticks += tr.Samples[after].DTS - tr.Samples[j].DTS
	}
	if tr.Timescale == 0 {
		return 0
	}
	if ticks == 0 {
		return 0
	}
	return ticks * 1_000_000 / int64(tr.Timescale)
}
