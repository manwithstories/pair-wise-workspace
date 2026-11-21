package pipeline

import (
	"context"
	"fmt"
	"sort"
	"time"

	"mediacheck/internal/index"
	"mediacheck/internal/model"
	"mediacheck/internal/probe"
	"mediacheck/internal/rtp"
	"mediacheck/internal/segment"
)

// probedInput pairs an input with its parsed result.
type probedInput struct {
	in  Input
	res *probe.Result
}

func (r *Runner) probeAll(ctx context.Context, inputs []Input) ([]probedInput, error) {
	out := make([]probedInput, 0, len(inputs))
	for _, in := range inputs {
		if err := ctx.Err(); err != nil {
			return nil, model.NewError(model.CodeCanceled, in.ID, 0, 0, "canceled during probe")
		}
		res, err := probe.Parse(in.Reader, in.Size, in.ID)
		if err != nil {
			return nil, fmt.Errorf("pipeline: probe %s: %w", in.ID, err)
		}
		out = append(out, probedInput{in: in, res: res})
	}
	// Stable ordering by representation name gives byte-stable manifests.
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].in.Representation < out[j].in.Representation
	})
	return out, nil
}

func (r *Runner) planAll(ctx context.Context, ps []probedInput) (
	segment.SharedManifestInput, []segment.AlignmentError, int64, error) {
	var anchor segment.TrackPlan
	var others []segment.TrackPlan
	var maxAVSync int64
	for i, p := range ps {
		if err := ctx.Err(); err != nil {
			return segment.SharedManifestInput{}, nil, 0,
				model.NewError(model.CodeCanceled, p.in.ID, 0, 0, "canceled during plan")
		}
		var video, audio *probe.Track
		for _, tr := range p.res.Tracks {
			if tr.IsVideo {
				video = tr
			} else {
				audio = tr
			}
		}
		if video == nil {
			return segment.SharedManifestInput{}, nil, 0,
				model.NewError(model.CodeIndexMissingPage, p.in.ID, 0, 0, "no video track")
		}
		tp := r.planner.PlanTrack(p.in.Representation, video)
		if audio != nil {
			var aligns []segment.AudioAlignment
			tp.Cuts, aligns = segment.AlignAudioTrack(audio, tp.Cuts, r.planner.MaxDrift())
			if ms := segment.MaxAVSyncMillis(aligns); ms > maxAVSync {
				maxAVSync = ms
			}
		}
		if i == 0 {
			anchor = tp
		} else {
			others = append(others, tp)
		}
	}
	aligned, errs := r.planner.Align(anchor, others)
	if err := segment.ValidateAlignment(aligned, r.planner.MaxDrift()); err != nil {
		return aligned, errs, maxAVSync, err
	}
	return aligned, errs, maxAVSync, nil
}

func (r *Runner) processSegments(ctx context.Context, ps []probedInput,
	aligned segment.SharedManifestInput, chunker *index.Chunker) (
	[]model.TimeRange, []int64, error) {

	quarantineSet := map[model.TimeRange]struct{}{}
	var quarantined []model.TimeRange
	var latencies []int64

	segs := segment.BuildSegments(aligned, nil)
	observed := 0

	// Pre-bucket every track's samples onto the shared segments so each
	// segment latency reading covers the full cross-representation workload.
	type trackBuckets struct {
		p       probedInput
		tr      *probe.Track
		buckets [][]uint32
	}
	var tbs []trackBuckets
	for _, p := range ps {
		for _, tr := range p.res.Tracks {
			tbs = append(tbs, trackBuckets{
				p: p, tr: tr, buckets: bucketSamplesBySegment(tr, segs),
			})
		}
	}

	for si := range segs {
		// One latency measurement per produced segment, spanning every
		// representation and track that contributes samples to it.
		procStart := time.Now()
		canceled := false
		pendingSave := false
		for _, tb := range tbs {
			for range tb.buckets[si] {
				if err := ctx.Err(); err != nil {
					return nil, latencies,
						model.NewError(model.CodeCanceled, tb.p.in.ID, 0, 0, "canceled mid-batch")
				}
				due := chunker.Observe(tb.p.in.ID, observed)
				observed++
				if due {
					pendingSave = true
				}
				if r.cfg.CancelAfterSamples > 0 &&
					chunker.Samples() >= r.cfg.CancelAfterSamples {
					canceled = true
					break
				}
			}
			if canceled {
				break
			}
		}
		// Nanosecond precision retained; rendered as fractional milliseconds.
		latencies = append(latencies, time.Since(procStart).Nanoseconds())
		if pendingSave {
			if err := chunker.Save(r.store); err != nil {
				return nil, latencies,
					model.NewError(model.CodeIndexWriteFailed, "checkpoint", 0, 0, err.Error())
			}
		}
		if canceled {
			return nil, latencies,
				model.NewError(model.CodeCanceled, "batch", 0, 0, "injected cancellation")
		}
	}

	// Defect-driven quarantine tails propagate probe findings across inputs.
	for _, p := range ps {
		tailEnd := lastSampleEnd(p.res)
		for _, me := range p.res.MediaErrors {
			if me.Code != model.CodeContainerTruncated &&
				me.Code != model.CodeLengthOutOfBounds &&
				me.Code != model.CodeIndexMissingPage {
				continue
			}
			q := model.TimeRange{Start: me.Start, End: me.End}
			if q.End <= q.Start && tailEnd > 0 {
				q.Start = tailEnd - 1
				q.End = tailEnd
			}
			if q.End > q.Start {
				if _, ok := quarantineSet[q]; !ok {
					quarantineSet[q] = struct{}{}
					quarantined = append(quarantined, q)
				}
			}
		}
	}
	for i := range segs {
		chunker.CommitSegment(fmt.Sprintf("seg-%d", segs[i].Index))
	}
	if err := chunker.Save(r.store); err != nil {
		return nil, latencies,
			model.NewError(model.CodeIndexWriteFailed, "checkpoint", 0, 0, err.Error())
	}
	sort.SliceStable(quarantined, func(i, j int) bool {
		return quarantined[i].Start < quarantined[j].Start
	})
	return quarantined, latencies, nil
}

func (r *Runner) writeManifest(digest string, aligned segment.SharedManifestInput,
	alignErrs []segment.AlignmentError, quarantined []model.TimeRange,
	rtpStats *rtp.Stats) (*segment.Manifest, error) {

	segs := segment.BuildSegments(aligned, quarantined)
	var view *segment.RTPStatsView
	if rtpStats != nil {
		view = &segment.RTPStatsView{
			Sent: rtpStats.Sent, Received: rtpStats.Received,
			Recovered: rtpStats.Recovered, Unrecoverable: rtpStats.Unrecoverable,
			RecoveryPercent: rtpStats.RecoveryPercent(),
		}
	}
	manifest := segment.FinalizeManifest(digest, aligned, segs, alignErrs, quarantined, view)
	if err := r.store.AtomicWrite("manifest.json", manifest); err != nil {
		return nil, err
	}
	return manifest, nil
}

// bucketSamplesBySegment groups sample indices by the segment whose
// half-open tick range contains their start tick.
func bucketSamplesBySegment(tr *probe.Track, segs []segment.Segment) [][]uint32 {
	out := make([][]uint32, len(segs))
	si := 0
	for _, s := range tr.Samples {
		for si < len(segs) && s.StartTicks >= segs[si].End {
			si++
		}
		if si >= len(segs) {
			si = len(segs) - 1
		}
		out[si] = append(out[si], s.Index)
	}
	return out
}

func lastSampleEnd(res *probe.Result) int64 {
	var end int64
	for _, tr := range res.Tracks {
		if len(tr.Samples) > 0 && tr.Samples[len(tr.Samples)-1].EndTicks > end {
			end = tr.Samples[len(tr.Samples)-1].EndTicks
		}
	}
	return end
}

// percentile95Micros computes P95 of per-segment processing latencies and
// returns it in milliseconds with sub-microsecond precision. The measurement
// is real wall-clock time; float rendering preserves values that integer
// millisecond rounding would otherwise hide as zero.
func percentile95Micros(latencyNanos []int64) float64 {
	if len(latencyNanos) == 0 {
		return 0
	}
	sorted := append([]int64(nil), latencyNanos...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	idx := (len(sorted) * 95) / 100
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return float64(sorted[idx]) / 1_000_000.0
}

func manifestDigest(m *segment.Manifest) string {
	digest, _, err := index.DigestJSON(m)
	if err != nil {
		return ""
	}
	return digest
}
