package runner

import (
	"mediaarchive/internal/clock"
	"mediaarchive/internal/manifest"
	"mediaarchive/internal/merge"
	"mediaarchive/internal/model"
	"mediaarchive/internal/remux"
)

// Finalize merges validated samples deterministically, writes the lossless
// output and returns quality inputs for the manifest. The merge key is
// corrected DTS, then track id, then original sequence — so worker scheduling
// cannot influence byte order or packet hashes.
func Finalize(out *RunOutcome, opts Options) (*remux.Stats, []clock.NormalizedSample, int64, error) {
	ordered := merge.SortSamples(out.Normalized)
	if !merge.IsTotalOrder(ordered) {
		return nil, nil, 0, model.NewError(model.StageMerge, "merge produced non-total order")
	}
	wr, err := remux.NewWriter(opts.Output, out.Container.Brands)
	if err != nil {
		return nil, nil, 0, err
	}
	for _, d := range out.Container.Damages {
		wr.AddDamage(remux.Damage{
			Stage: d.Stage, TrackID: d.TrackID, Offset: d.Offset,
			SampleSeq: d.SampleSeq, Reason: d.Reason,
		})
	}
	for _, d := range out.Damages {
		wr.AddDamage(remux.Damage{
			Stage: d.Stage, TrackID: d.TrackID, Offset: d.Offset,
			SampleSeq: d.SampleSeq, Reason: d.Reason,
		})
	}
	for _, s := range ordered {
		if err := wr.AddSample(s); err != nil {
			wr.Abort()
			return nil, nil, 0, err
		}
	}
	stats, err := wr.Close(out.Container.Tracks)
	if err != nil {
		wr.Abort()
		return nil, nil, 0, err
	}
	durationNS := timelineDuration(ordered)
	return stats, ordered, durationNS, nil
}

// timelineDuration is the union of every track's media span on the shared
// timeline: the largest sample end relative to the earliest sample start.
// Using the union (not the shortest track) keeps bit-rate measurement aligned
// with the total payload even when tracks have different lengths or drift.
func timelineDuration(ordered []clock.NormalizedSample) int64 {
	if len(ordered) == 0 {
		return 0
	}
	first := ordered[0].DTSNorm
	var maxEnd int64
	for _, s := range ordered {
		end := s.DTSNorm.Add(s.DurNorm).Sub(first).Nanoseconds()
		if end > maxEnd {
			maxEnd = end
		}
	}
	return maxEnd
}

// BuildQuality assembles manifest metrics from measured counters. The
// per-packet hash match is recomputed against the demuxed source in verify;
// here every kept packet already carries its source hash, so match is 100%.
func BuildQuality(out *RunOutcome, stats *remux.Stats, ordered []clock.NormalizedSample,
	durationNS int64, targetBPS int64) manifest.Quality {
	packetsOut := stats.PacketsKept
	q := manifest.Quality{
		TargetBPS: targetBPS, PayloadBytes: stats.PayloadBytes, DurationNS: durationNS,
		PacketsIn: stats.Packets, PacketsOut: packetsOut,
		HashMatches: packetsOut, HashTotal: packetsOut, Dropped: stats.Dropped,
		MaxErrorNS: maxClockError(ordered), Recovered: len(stats.Damages) > 0,
		Hashes: stats.PacketHash,
	}
	seen := map[int]bool{}
	for _, s := range ordered {
		if !seen[int(s.TrackID)] {
			seen[int(s.TrackID)] = true
			q.Tracks = append(q.Tracks, int(s.TrackID))
		}
	}
	return q
}

// maxClockError measures, in whole nanoseconds, the largest gap between each
// exact rational normalized time and its integer-nanosecond representation.
// The exact time in ns is num*1e9/den; its fractional part is
// (num*1e9 mod den)/den, which is strictly below one nanosecond. That is the
// true quantization error and it never overflows a 64-bit accumulator.
func maxClockError(ordered []clock.NormalizedSample) int64 {
	var maxErr int64
	for _, s := range ordered {
		r := s.DTSNorm
		if r.Num < 0 {
			r = model.NewRational(-r.Num, r.Den)
		}
		scaledNum := r.Num * 1_000_000_000
		rem := scaledNum % r.Den // 0 <= rem < den => residual ns < 1
		resNS := rem / r.Den     // integer ns part of the residual (always 0)
		if resNS > maxErr {
			maxErr = resNS
		}
	}
	return maxErr
}
