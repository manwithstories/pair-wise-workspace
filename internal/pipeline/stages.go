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
	for _, p := range ps {
		for _, tr := range p.res.Tracks {
			for range tr.Samples {
				if err := ctx.Err(); err != nil {
					return nil, latencies,
						model.NewError(model.CodeCanceled, p.in.ID, 0, 0, "canceled mid-batch")
				}
				start := time.Now()
				due := chunker.Observe(p.in.ID, observed)
				observed++
				latencies = append(latencies, time.Since(start).Microseconds()/10)
				if r.cfg.CancelAfterSamples > 0 &&
					chunker.Samples() >= r.cfg.CancelAfterSamples {
					return nil, latencies,
						model.NewError(model.CodeCanceled, p.in.ID, 0, 0, "injected cancellation")
				}
				if due {
					if err := chunker.Save(r.store); err != nil {
						return nil, latencies,
							model.NewError(model.CodeIndexWriteFailed, p.in.ID, 0, 0, err.Error())
					}
				}
			}
			// Defect-driven quarantine tails propagate probe findings.
			// Byte-offset defects may carry a zero span; the last known
			// sample timeline position supplies a positive quarantine span.
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

func lastSampleEnd(res *probe.Result) int64 {
	var end int64
	for _, tr := range res.Tracks {
		if len(tr.Samples) > 0 && tr.Samples[len(tr.Samples)-1].EndTicks > end {
			end = tr.Samples[len(tr.Samples)-1].EndTicks
		}
	}
	return end
}

func percentile95(latencyMillis []int64) int64 {
	if len(latencyMillis) == 0 {
		return 0
	}
	sorted := append([]int64(nil), latencyMillis...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	idx := (len(sorted) * 95) / 100
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

func manifestDigest(m *segment.Manifest) string {
	digest, _, err := index.DigestJSON(m)
	if err != nil {
		return ""
	}
	return digest
}
