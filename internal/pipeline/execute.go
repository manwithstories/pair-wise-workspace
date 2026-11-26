package pipeline

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"sort"

	"mediad/internal/av"
	"mediad/internal/clock"
	"mediad/internal/manifest"
	"mediad/internal/media"
)

// Run executes the plan and publishes one delivery file under opts.OutDir.
// Output is written to a temp file, verified, then atomically renamed, so an
// interrupt can never overwrite a previously verified delivery.
func Run(opts Options) (*Stats, error) {
	plan, err := BuildPlan(opts)
	if err != nil {
		return nil, err
	}
	return RunPlan(opts, plan)
}

// RunPlan renders a prebuilt plan.
func RunPlan(opts Options, plan *Plan) (*Stats, error) {
	if opts.OutDir == "" {
		return nil, media.NewError(media.ErrIO, "output directory is required")
	}
	if err := os.MkdirAll(opts.OutDir, 0o755); err != nil {
		return nil, media.NewError(media.ErrIO, "creating output directory: %v", err)
	}
	outPath := filepath.Join(opts.OutDir, "delivery.m4a")
	cpPath := filepath.Join(opts.OutDir, "checkpoint.json")
	planHash := planFingerprint(opts, plan)

	// Resume: if a verified delivery already matches this plan, reuse it.
	if opts.Resume {
		if cp, err := LoadCheckpoint(cpPath); err == nil && cp != nil && cp.PlanHash == planHash && cp.Stats != nil {
			if VerifyDelivery(outPath, cp.Stats.OutputSHA256) == nil {
				return cp.Stats, nil
			}
		}
	}

	stats := &Stats{
		TargetKbps: av.TargetKbps,
		LateEvents: plan.Late,
		DriftPPM:   plan.Drift.WorstPPM,
	}
	stats.ReplayedEvents = plan.Replayed
	stats.DriftAnomaly = plan.Drift.Anomaly

	enc, err := av.NewEncoderAAC(outPath, av.OutputRate, av.TargetKbps)
	if err != nil {
		return nil, err
	}
	defer enc.Abort()

	// Pass one: render each segment in turn, feed a streaming loudness meter
	// and keep only per-segment metadata. The programme is never materialised
	// in full, so memory stays bounded by one segment (<=5 minutes).
	meter := av.NewLoudnessMeter()
	alignMax := 0.0
	damaged := 0
	var damagedDetail []DamageRecord
	var totalSamples int

	render := func(apply func([]float32) error, collect bool) error {
		for i, seg := range plan.Segments {
			streamIndex, err := resolveStreamIndex(seg)
			if err != nil {
				return err
			}
			if seg.AlignmentUS > alignMax {
				alignMax = seg.AlignmentUS
			}
			matrix := seg.Entry.MixMatrix
			var mm av.MixMatrix
			if len(matrix) == 2 {
				mm = av.MixMatrix{matrix[0], matrix[1]}
			}
			res, err := av.ReadSegment(av.SegmentRequest{
				Path:        seg.Path,
				StreamIndex: streamIndex,
				TimeBase:    seg.Entry.TimeBase(),
				InRaw:       seg.Event.In,
				OutRaw:      seg.Event.Out,
				InLayout:    seg.Entry.Layout,
				InChannels:  seg.Entry.Channels,
				Matrix:      mm,
			})
			if err != nil {
				return err
			}
			if collect {
				damaged += len(res.Damaged)
				for _, d := range res.Damaged {
					damagedDetail = append(damagedDetail, DamageRecord{
						Camera: seg.Event.Camera, Track: seg.Event.Track,
						StartRaw: d.StartRaw, EndRaw: d.EndRaw, Reason: d.Reason,
					})
				}
				if res.AlignmentUS > alignMax {
					alignMax = res.AlignmentUS
				}
				totalSamples += res.LengthSamples
			}
			if apply != nil {
				if err := apply(res.Samples); err != nil {
					return err
				}
			}
			if collect {
				if err := SaveCheckpoint(cpPath, Checkpoint{PlanHash: planHash, Completed: []string{segKey(seg)}, Partial: enc.TempPath()}); err != nil {
					return err
				}
			}
			_ = i
		}
		return nil
	}

	if err := render(func(s []float32) error { meter.Push(s); return nil }, true); err != nil {
		return nil, err
	}
	if totalSamples == 0 {
		return nil, media.NewError(media.ErrManifest, "plan produced no audio")
	}

	// Compute a peak-safe gain from the first-pass measurement, then render a
	// second time applying it and writing straight to the encoder.
	measured := meter.Report()
	gain := av.NormalizeGain(measured)
	finalMeter := av.NewLoudnessMeter()
	if err := render(func(s []float32) error {
		av.ApplyGain(s, gain)
		finalMeter.Push(s)
		return enc.WriteSamples(s)
	}, false); err != nil {
		return nil, err
	}
	report := finalMeter.Report()
	if err := enc.Close(); err != nil {
		return nil, err
	}
	seconds := float64(totalSamples) / av.OutputRate
	payload := enc.PayloadBytes()
	actualKbps := float64(payload) * 8 / seconds / 1000

	stats.ActualKbps = actualKbps
	stats.BitrateErrorPct = media.Percent(actualKbps, float64(av.TargetKbps))
	stats.IntegratedLUFS = report.IntegratedLUFS
	stats.TruePeakDBTP = report.TruePeakDBTP
	stats.ClippedSamples = report.ClippedSamples
	stats.SilentSeconds = report.SilentSeconds
	stats.ManifestAlignUS = alignMax
	stats.DurationSeconds = seconds
	stats.DamagedRanges = damaged
	stats.Damaged = damagedDetail

	// Verify the completed temp file, then publish atomically.
	if err := VerifyDelivery(enc.TempPath(), ""); err != nil {
		return nil, err
	}
	// Re-render the report from the actual encoded file's hash before rename.
	hash, err := HashFile(enc.TempPath())
	if err != nil {
		return nil, err
	}
	stats.OutputSHA256 = hash
	stats.OutputPath = outPath
	if err := enc.Commit(); err != nil {
		return nil, err
	}
	if err := SaveCheckpoint(cpPath, Checkpoint{PlanHash: planHash, Completed: []string{"all"}, Stats: stats}); err != nil {
		return nil, err
	}
	return stats, nil
}

// streamIndexFor returns the declared audio stream index, or -1 to let the
// caller probe the first audio stream.
func streamIndexFor(e *manifest.Entry) int {
	if e.StreamIndex != nil {
		return *e.StreamIndex
	}
	return -1
}

func segKey(seg Segment) string {
	return seg.Event.Camera + "/" + seg.Event.Track + "/" + media.FormatInt(seg.Event.Seq)
}

// planFingerprint hashes the normalised plan so a resumed run can tell whether
// its checkpoint still describes the current input.
func planFingerprint(opts Options, plan *Plan) string {
	h := sha256.New()
	for _, seg := range plan.Segments {
		h.Write([]byte(seg.Path))
		h.Write([]byte(seg.Event.Camera))
		h.Write([]byte(seg.Event.Track))
		h.Write([]byte(media.FormatInt(seg.Event.In)))
		h.Write([]byte(media.FormatInt(seg.Event.Out)))
		h.Write([]byte(media.FormatInt(seg.Event.Seq)))
		h.Write([]byte(seg.Entry.Timebase))
		h.Write([]byte(string(rune(seg.Entry.WrapBits))))
	}
	for _, e := range opts.Manifest.Entries {
		for _, a := range e.Anchors {
			h.Write([]byte(media.FormatInt(a.HostUS)))
			h.Write([]byte(media.FormatInt(a.MediaTicks)))
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

// resolveStreamIndex returns the manifest-declared stream, probing the first
// audio stream when the manifest omits it.
func resolveStreamIndex(seg Segment) (int, error) {
	if idx := streamIndexFor(seg.Entry); idx >= 0 {
		return idx, nil
	}
	d, err := av.NewDemuxer(seg.Path)
	if err != nil {
		return 0, err
	}
	infos, err := d.ProbeStreams()
	if err != nil {
		return 0, err
	}
	if len(infos) == 0 {
		return 0, media.NewError(media.ErrUnsupported, "%s has no audio stream", seg.Path)
	}
	return infos[0].StreamIndex, nil
}

// SortedCameras lists manifest cameras deterministically (diagnostics).
func SortedCameras(m *manifest.Manifest) []string {
	set := map[string]struct{}{}
	for _, e := range m.Entries {
		set[e.Camera] = struct{}{}
	}
	out := make([]string, 0, len(set))
	for c := range set {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// FitString renders a fit for diagnostics.
func FitString(f *clock.Fit) string { return f.String() }
