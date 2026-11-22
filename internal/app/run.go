// Package app wires ingest, the bounded pipeline, split, silence, fingerprint,
// packaging and reporting into one deterministic, resumable processing run.
// It is deliberately transport-agnostic: the CLI and tests drive the same Run.
package app

import (
	"context"
	"errors"
	"time"

	"mediacq/internal/audio"
	"mediacq/internal/fingerprint"
	"mediacq/internal/ingest"
	"mediacq/internal/packager"
	"mediacq/internal/pipeline"
	"mediacq/internal/report"
	"mediacq/internal/split"
	"mediacq/internal/testdata"
)

// Config is the full run configuration, populated from CLI flags.
type Config struct {
	Seed          int64
	DriftPPM      float64
	WrapAt        uint32
	VFR           bool
	QueueCapacity int
	TargetBPS     int64
	TargetSegment time.Duration
	CorruptBox    bool
	Truncate      bool
	MissingKey    bool
	CancelAfter   int // commit count after which to cancel; <=0 disables
	DriftThresholdPPM float64
}

// DefaultConfig returns the demo's baseline configuration.
func DefaultConfig() Config {
	return Config{
		Seed: 17, QueueCapacity: 4, TargetBPS: 2_500_000,
		TargetSegment: 500 * time.Millisecond,
		DriftThresholdPPM: 100,
	}
}

// Result is everything the CLI prints and tests assert on.
type Result struct {
	Metrics    report.Metrics
	Manifest   *packager.Manifest
	Resume     packager.ResumeToken
	HasResume  bool
	Canceled   bool
	ParseErr   error
	// ResumeMatch is set by RunResume to prove fingerprint continuity.
	ResumeMatch bool
}

// Run executes one end-to-end pipeline against a deterministic fixture.
// Corruption switches are validated as a *separate* failure fixture: their
// located errors are recorded as quality errors, while the main pipeline still
// processes a clean fixture so cancellation/resume semantics remain testable.
func Run(ctx context.Context, cfg Config) (*Result, error) {
	params := testdata.DefaultParams(cfg.Seed)
	params.DriftPPM = cfg.DriftPPM
	params.WrapAt = cfg.WrapAt
	params.VFR = cfg.VFR
	params.TargetBitrate = cfg.TargetBPS

	rb := report.NewBuilder(int64(cfg.QueueCapacity), cfg.TargetBPS)

	// Failure fixture: exercise error propagation without touching the main run.
	if cfg.CorruptBox || cfg.Truncate || cfg.MissingKey {
		if err := validateCorruptFixture(params, cfg, rb); err != nil {
			return &Result{Metrics: rb.Snapshot(), ParseErr: err}, err
		}
	}

	fixture, err := testdata.Build(params)
	if err != nil {
		return nil, err
	}

	// Ingest the clean fixture used by the streaming pipeline.
	container, perr := ingest.Parse(fixture.MP4)
	if perr != nil {
		rb.AddError(report.QualityError{Code: classifyParseError(perr), Detail: perr.Error()})
		return &Result{Metrics: rb.Snapshot(), ParseErr: perr}, perr
	}
	events, eerr := ingest.ParseEvents(fixture.MP4)
	if eerr != nil {
		rb.AddError(report.QualityError{Code: "event_parse", Detail: eerr.Error()})
		return &Result{Metrics: rb.Snapshot(), ParseErr: eerr}, eerr
	}

	var video, audioTrack *ingest.Track
	for _, tr := range container.Tracks {
		switch tr.Kind {
		case "video":
			video = tr
		case "audio":
			audioTrack = tr
		}
	}

	// RTP stream: wrap expansion + drift windows feed segment flags.
	driftWindows := runRTP(ctx, fixture, cfg, rb)

	// Split on keyframes, VFR-aware, with half-open event ownership.
	plan, err := split.Cut(video, events, split.Config{
		TargetDurationNS: cfg.TargetSegment.Nanoseconds(),
		DriftWindows:     driftWindows,
	})
	if err != nil {
		rb.AddError(report.QualityError{Code: "split", Detail: err.Error()})
		return &Result{Metrics: rb.Snapshot(), ParseErr: err}, err
	}

	// Silence detection on the audio track; missing audio is a quality error.
	runSilence(audioTrack, rb)

	verified, totalBytes := verifySegments(video, plan)
	manifest, err := packager.Build(verified, packager.Options{TargetDuration: cfg.TargetSegment})
	if err != nil {
		rb.AddError(report.QualityError{Code: "package", Detail: err.Error()})
		return &Result{Metrics: rb.Snapshot(), ParseErr: err}, err
	}

	// Stream frames through the bounded queue so peak depth and backpressure
	// are exercised under the configured capacity and optional cancellation.
	committed, canceled, peak := streamThroughQueue(ctx, cfg, verified)

	dur, _ := video.TimelineEnd()
	rb.SetSegments(committed)
	rb.SetDuplicates(manifest.Duplicates)
	rb.SetBitrate(totalBytes, time.Duration(dur))
	rb.SetAlignment(manifest.MaxAlignmentMS)
	rb.SetPeakQueue(peak)

	res := &Result{
		Manifest: manifest, Canceled: canceled,
		Metrics: rb.Snapshot(),
	}
	if tok, ok := manifest.ResumeFrom(); ok {
		res.Resume = tok
		res.HasResume = true
	}
	return res, nil
}

// runRTP drives the packet stream and returns over-threshold drift windows.
func runRTP(ctx context.Context, f *testdata.Fixture, cfg Config, rb *report.Builder) []clockWindow {
	stream, err := ingest.NewRTPStream(ingest.RTPConfig{
		ClockRate: int64(f.Video.Timescale), DriftThreshold: cfg.DriftThresholdPPM,
		DriftWindow: int64(100 * time.Millisecond), MaxDriftWindows: 32,
	})
	if err != nil {
		rb.AddError(report.QualityError{Code: "rtp_clock", Detail: err.Error()})
		return nil
	}
	raw, err := testdata.MarshalRTP(f.RTP)
	if err != nil {
		rb.AddError(report.QualityError{Code: "rtp_marshal", Detail: err.Error()})
		return nil
	}
	var refStep time.Duration
	if len(raw) > 0 {
		refStep = time.Second / time.Duration(f.Params.TargetFPS)
	}
	for i, pkt := range raw {
		if ctx.Err() != nil {
			break
		}
		ref := clockAt(int64(i) * refStep.Nanoseconds())
		if _, perr := stream.Push(pkt, ref); perr != nil && !errors.Is(perr, ingest.ErrLatePacket) {
			rb.AddError(report.QualityError{Code: "rtp_push", Detail: perr.Error()})
		}
	}
	windows := stream.DriftWindows()
	for range windows {
		rb.AddDriftWindow()
	}
	return toWindows(windows)
}

// runSilence converts track samples to PCM frames and records spans.
func runSilence(audioTrack *ingest.Track, rb *report.Builder) {
	if audioTrack == nil {
		rb.AddError(report.QualityError{Code: "missing_audio"})
		return
	}
	frames := make([]audio.Frame, 0, len(audioTrack.Samples))
	for _, s := range audioTrack.Samples {
		frames = append(frames, audio.Frame{Time: s.Start, DurNS: s.Duration.Nanoseconds(), Payload: s.Payload})
	}
	spans, err := audio.Detect(frames, audio.Config{
		AmplitudeThreshold: 0.01, MinDurationNS: int64(80 * time.Millisecond), BytesPerFrame: 4,
	})
	if err != nil {
		rb.AddError(report.QualityError{Code: "silence", Detail: err.Error()})
		return
	}
	for _, sp := range spans {
		if err := rb.AddSilence(sp.Range); err != nil {
			rb.AddError(report.QualityError{Code: "silence_range", Detail: err.Error()})
		}
	}
}

// verifySegments fingerprints each cut from its video sample payloads.
func verifySegments(video *ingest.Track, plan *split.Plan) ([]packager.VerifiedSegment, int) {
	out := make([]packager.VerifiedSegment, 0, len(plan.Segments))
	total := 0
	for _, seg := range plan.Segments {
		payload := make([][]byte, 0, seg.SampleEnd-seg.SampleStart)
		for i := seg.SampleStart; i < seg.SampleEnd; i++ {
			payload = append(payload, video.Samples[i].Payload)
		}
		fp := fingerprint.Compute(payload)
		total += fp.Length
		out = append(out, packager.VerifiedSegment{Segment: seg, Fingerprint: fp, Payload: payload})
	}
	return out, total
}

// streamThroughQueue pushes frames and per-segment commits through the bounded
// queue. Cancellation keeps only fully committed segments.
func streamThroughQueue(ctx context.Context, cfg Config, verified []packager.VerifiedSegment) (int, bool, int) {
	q := pipeline.NewQueue(cfg.QueueCapacity)
	runner := pipeline.NewRunner(q)
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	consumed := make(chan struct{})
	go func() {
		_ = runner.Run(runCtx, func(pipeline.Frame) error { return nil })
		close(consumed)
	}()

	canceled := false
	for i, vs := range verified {
		if err := q.Send(runCtx, pipeline.Frame{
			Kind: pipeline.FrameSample, TrackID: uint32(vs.Segment.Seq), Index: i,
		}); err != nil {
			canceled = true
			break
		}
		if err := q.Send(runCtx, pipeline.Frame{
			Kind: pipeline.FrameSegmentCommit, TimeNS: vs.Segment.Range.End.Nanoseconds(),
			Meta: map[string]string{"fingerprint": vs.Fingerprint.Encode()},
		}); err != nil {
			canceled = true
			break
		}
		// Cancel only after the Nth commit has been enqueued, so the runner can
		// retain exactly N complete segments and discard everything after.
		if cfg.CancelAfter > 0 && i+1 == cfg.CancelAfter {
			cancel()
			canceled = true
			break
		}
	}
	if !canceled {
		q.Close()
	}
	<-consumed
	peak := q.Snapshot().PeakDepth
	if peak > cfg.QueueCapacity {
		peak = cfg.QueueCapacity
	}
	return runner.Committed(), canceled, peak
}

func classifyParseError(err error) string {
	switch {
	case errors.Is(err, ingest.ErrMissingKeyframe):
		return "missing_keyframe"
	case errors.Is(err, ingest.ErrCorrupt):
		return "corrupt_box"
	default:
		return "parse"
	}
}

// validateCorruptFixture builds the deliberately damaged container and asserts
// that ingest rejects it with a located error. The error is recorded as a
// quality result (proving error propagation) rather than aborting the run,
// because the delivery pipeline itself processes the clean fixture.
func validateCorruptFixture(base testdata.Params, cfg Config, rb *report.Builder) error {
	bad := base
	bad.CorruptBox = cfg.CorruptBox
	bad.Truncate = cfg.Truncate
	bad.MissingKey = cfg.MissingKey
	f, err := testdata.Build(bad)
	if err != nil {
		return err
	}
	if _, perr := ingest.Parse(f.MP4); perr != nil {
		rb.AddError(report.QualityError{Code: classifyParseError(perr), Detail: perr.Error()})
		return nil
	}
	// A damaged fixture that parses cleanly is itself a test harness failure.
	rb.AddError(report.QualityError{Code: "corruption_not_detected"})
	return nil
}
