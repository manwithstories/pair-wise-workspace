package demo

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"avpipe/internal/graph"
	"avpipe/internal/layout"
	"avpipe/internal/media"
	"avpipe/internal/timeline"
	"avpipe/internal/wav"
)

// Options are the demo parameters parsed by the CLI.
type Options struct {
	Seed             int64
	Queue            int
	ConsumerStallMS  int
	CorruptTailBytes int
	VerifyRepeat     bool
	Out              string
	Frames           int

	// nodeOverride is an in-package test seam: it replaces the stage graph
	// with an arbitrary one so negative controls (cycles, missing tracks,
	// out-of-range spans) can prove that validation fails before any output
	// file exists. External callers never set it.
	nodeOverride func(anchors int) []graph.Node
}

// DefaultOptions mirror the acceptance command line.
func DefaultOptions() Options {
	return Options{
		Seed:             42,
		Queue:            4,
		ConsumerStallMS:  30,
		CorruptTailBytes: 7,
		VerifyRepeat:     true,
		Frames:           44100, // one second of 44.1 kHz source material
	}
}

// Validate rejects parameters that cannot produce a meaningful run.
func (o Options) Validate() error {
	if o.Queue <= 0 {
		return media.NewError(media.PhaseConfig, media.ExitUsage, "", 0, false, "options",
			fmt.Errorf("--queue must be positive, got %d", o.Queue))
	}
	if o.ConsumerStallMS < 0 {
		return media.NewError(media.PhaseConfig, media.ExitUsage, "", 0, false, "options",
			fmt.Errorf("--consumer-stall-ms must be non-negative"))
	}
	if o.CorruptTailBytes < 0 {
		return media.NewError(media.PhaseConfig, media.ExitUsage, "", 0, false, "options",
			fmt.Errorf("--corrupt-tail-bytes must be non-negative"))
	}
	if o.Frames <= 0 || o.Frames > 44100*60 {
		return media.NewError(media.PhaseConfig, media.ExitUsage, "", 0, false, "options",
			fmt.Errorf("--frames must be in 1..%d", 44100*60))
	}
	if o.Out == "" {
		return media.NewError(media.PhaseConfig, media.ExitUsage, "", 0, false, "options",
			fmt.Errorf("--out is required"))
	}
	if o.CorruptTailBytes >= o.Frames*len(layout.Surround.Labels)*2 {
		return media.NewError(media.PhaseConfig, media.ExitUsage, "", 0, false, "options",
			fmt.Errorf("--corrupt-tail-bytes exceeds the fixture size"))
	}
	return nil
}

// Report is the machine-checked outcome printed to the terminal.
type Report struct {
	RoundTripPct   float64
	KeyframePct    float64
	SegmentCount   int
	SegmentSHA     []uint64
	QueueCapacity  int
	QueueHighWater int
	QueueFullSeen  bool
	Backpressure   uint64
	RecoveredBytes int64
	RepeatMatch    float64
	RepeatEnabled  bool
	StrictRejected bool
	LoudnessDBFS   float64
	PeakDBFS       float64
	Digest         string
	RepeatDigest   string
	Gaps           uint64
	MissingFrames  int64
	Duplicates     uint64
	Elapsed        time.Duration
	OutPath        string
	FixtureDir     string
}

// Passed reports whether every acceptance criterion holds.
func (r Report) Passed() bool {
	if r.RoundTripPct < 100 || r.KeyframePct < 100 {
		return false
	}
	if r.SegmentCount == 0 || len(r.SegmentSHA) != r.SegmentCount {
		return false
	}
	if r.QueueHighWater > r.QueueCapacity || r.QueueCapacity <= 0 {
		return false
	}
	if r.Backpressure == 0 || !r.QueueFullSeen {
		return false
	}
	if r.RecoveredBytes <= 0 || !r.StrictRejected {
		return false
	}
	if r.RepeatEnabled && r.RepeatMatch < 100 {
		return false
	}
	if r.PeakDBFS > -1 {
		return false
	}
	// -20 dBFS +/-1 dB. The controller is adaptive and starts transparent, so
	// short fixtures are judged with the same tolerance band as long ones.
	if r.LoudnessDBFS < -21 || r.LoudnessDBFS > -19 {
		return false
	}
	return true
}

// Run executes the demo and returns the report.
func Run(ctx context.Context, opts Options, stdout, stderr *os.File) (Report, error) {
	start := time.Now()
	if err := opts.Validate(); err != nil {
		return Report{}, err
	}
	fmt.Fprintf(stdout, "avpipe demo seed=%d queue=%d stall_ms=%d corrupt_tail=%d repeat=%v\n",
		opts.Seed, opts.Queue, opts.ConsumerStallMS, opts.CorruptTailBytes, opts.VerifyRepeat)

	if err := os.MkdirAll(opts.Out, 0o755); err != nil {
		return Report{}, media.NewError(media.PhaseIO, media.ExitInternal, "", 0, false, "mkdir", err)
	}
	fixDir := filepath.Join(opts.Out, "fixtures")
	fx, err := GenerateFixtures(fixDir, opts.Seed, opts.Frames, opts.CorruptTailBytes)
	if err != nil {
		return Report{}, media.NewError(media.PhaseIO, media.ExitInternal, "", 0, false, "fixtures", err)
	}
	fmt.Fprintf(stdout, "fixtures: %s (%d frames @ %s, %d ch, PCM16), truncated=%s, anchors=%d\n",
		fx.Normal, fx.Frames, fx.Rate.String(), len(fx.Labels), filepath.Base(fx.Truncated), len(fx.Anchors))

	tl, err := buildTimeline(fx)
	if err != nil {
		return Report{}, err
	}
	if err := validateGraphWith(tl, opts, len(fx.Anchors)); err != nil {
		return Report{}, err
	}

	// First pass renders to a deterministic temporary file; the atomic rename
	// installs it only after the run and its audit both succeed.
	finalPath := filepath.Join(opts.Out, "session.wav")
	tmpPath := finalPath + ".tmp-deterministic"
	stats, err := runOnce(ctx, fx, opts, tl, tmpPath)
	if err != nil {
		// The deterministic temporary is never installed on failure, and it
		// must not linger to confuse a later recovery run.
		os.Remove(tmpPath)
		return Report{}, err
	}
	newTL, err := buildTimeline(fx)
	if err != nil {
		return Report{}, err
	}
	firstDigest, err := digestOf(tmpPath)
	if err != nil {
		return Report{}, media.NewError(media.PhaseIO, media.ExitInternal, "", 0, false, "digest", err)
	}

	report := Report{
		RoundTripPct:   stats.summary.RoundTripPct,
		KeyframePct:    stats.summary.KeyframePct,
		SegmentCount:   stats.summary.SegmentCount,
		SegmentSHA:     stats.segmentHashes(),
		QueueCapacity:  stats.result.Queue.Capacity,
		QueueHighWater: stats.result.Queue.HighWater,
		QueueFullSeen:  stats.gateSat,
		Backpressure:   stats.result.Queue.Backpressure,
		LoudnessDBFS:   stats.loudnessDBFS,
		PeakDBFS:       stats.peakDBFS,
		Digest:         stats.digest,
		Gaps:           stats.result.Gaps,
		MissingFrames:  stats.result.GapFrames,
		Duplicates:     stats.result.Duplicates,
		OutPath:        finalPath,
		FixtureDir:     fixDir,
	}

	strictOK, err := strictReject(fx)
	if err != nil {
		return Report{}, err
	}
	report.StrictRejected = strictOK
	rec, err := recoveryPass(fx)
	if err != nil {
		return Report{}, err
	}
	report.RecoveredBytes = rec

	report.RepeatEnabled = opts.VerifyRepeat
	if opts.VerifyRepeat {
		repeatPath := filepath.Join(opts.Out, "session-repeat.wav")
		repeatStats, err := runOnce(ctx, fx, opts, newTL, repeatPath)
		if err != nil {
			return Report{}, err
		}
		secondDigest, err := digestOf(repeatPath)
		if err != nil {
			return Report{}, err
		}
		report.RepeatDigest = repeatStats.digest
		report.RepeatMatch = comparePercent(firstDigest[:], secondDigest[:])
		if err := os.Remove(repeatPath); err != nil {
			return Report{}, err
		}
	}

	if err := atomicInstall(tmpPath, finalPath); err != nil {
		return Report{}, err
	}
	report.Elapsed = time.Since(start)
	printReport(stdout, report)
	return report, nil
}

// buildTimeline registers the program audio track and the generated video
// anchors on the shared clock.
func buildTimeline(fx Fixtures) (*timeline.Timeline, error) {
	tl, err := timeline.New(fx.Rate)
	if err != nil {
		return nil, err
	}
	anchors := make([]timeline.Anchor, len(fx.Anchors))
	for i, a := range fx.Anchors {
		anchors[i] = timeline.Anchor{At: a.At, Label: a.Label}
	}
	if err := tl.Add(timeline.Track{
		ID: "program", Kind: timeline.Audio, Rate: fx.Rate,
		Channels: len(fx.Labels), Labels: fx.Labels,
	}); err != nil {
		return nil, err
	}
	if err := tl.Add(timeline.Track{ID: "cam", Kind: timeline.Video, Anchors: anchors}); err != nil {
		return nil, err
	}
	if err := tl.Finalize(); err != nil {
		return nil, err
	}
	return tl, nil
}

// validateGraph proves the default stage dependency graph is acyclic and its
// keyframe span is in range before any audio is written.
func validateGraph(tl *timeline.Timeline, anchors int) error {
	return validateGraphWith(tl, Options{}, anchors)
}

// validateGraphWith validates the graph selected by opts, including the
// in-package test override.
func validateGraphWith(tl *timeline.Timeline, opts Options, anchors int) error {
	nodes := planNodes(anchors)
	if opts.nodeOverride != nil {
		nodes = opts.nodeOverride(anchors)
	}
	_, err := graph.Build(nodes, tl.IDs(), anchors)
	return err
}

// planNodes is the production stage graph: source, resample, layout,
// loudness, sink and the audit consumer of the video anchors.
func planNodes(anchors int) []graph.Node {
	return []graph.Node{
		{ID: "program.source", Stage: graph.StageSource, Track: "program"},
		{ID: "program.resample", Stage: graph.StageResample, Track: "program", Deps: []string{"program.source"}},
		{ID: "program.layout", Stage: graph.StageLayout, Track: "program", Deps: []string{"program.resample"}},
		{ID: "program.loudness", Stage: graph.StageLoudness, Track: "program", Deps: []string{"program.layout"}},
		{ID: "program.sink", Stage: graph.StageSink, Track: "program", Deps: []string{"program.loudness"}},
		{ID: "cam.audit", Stage: graph.StageAudit, Track: "cam", Deps: []string{"program.sink"},
			Anchors: &graph.Span{Lo: 0, Hi: anchors}},
	}
}

// recoveryPass reads the truncated fixture non-strictly and returns the bytes
// that were dropped because they did not form a complete decoded frame. The
// reported figure is the physically missing tail (the corrupt-tail-bytes
// parameter), not the fragment that happened to be present.
func recoveryPass(fx Fixtures) (int64, error) {
	reader, err := wav.Open(fx.Truncated, wav.DefaultLimits())
	if err != nil {
		return 0, err
	}
	defer reader.Close()
	var dropped int64
	for {
		chunk, err := reader.ReadBlock("program", 1024, false)
		if err != nil {
			break
		}
		if chunk.DroppedBytes > 0 {
			dropped += chunk.DroppedBytes
		}
	}
	if dropped == 0 {
		return 0, media.NewError(media.PhaseInput, media.ExitInput, "program", 0, false, "recovery",
			fmt.Errorf("recovery pass recovered no bytes"))
	}
	return dropped, nil
}

func comparePercent(a, b []byte) float64 {
	if len(a) == 0 || len(a) != len(b) {
		return 0
	}
	equal := 0
	for i := range a {
		if a[i] == b[i] {
			equal++
		}
	}
	return 100 * float64(equal) / float64(len(a))
}

func printReport(w *os.File, r Report) {
	fmt.Fprintf(w, "roundtrip_match=%.2f%%\n", r.RoundTripPct)
	fmt.Fprintf(w, "keyframe_hit=%.2f%%\n", r.KeyframePct)
	fmt.Fprintf(w, "segment_count=%d\n", r.SegmentCount)
	for i, h := range r.SegmentSHA {
		fmt.Fprintf(w, "segment_sha256[%d]=%016x\n", i, h)
	}
	fmt.Fprintf(w, "queue_capacity=%d\n", r.QueueCapacity)
	fmt.Fprintf(w, "queue_high_water=%d\n", r.QueueHighWater)
	fmt.Fprintf(w, "queue_full_seen=%v\n", r.QueueFullSeen)
	fmt.Fprintf(w, "backpressure_waits=%d\n", r.Backpressure)
	fmt.Fprintf(w, "recovered_dropped_bytes=%d\n", r.RecoveredBytes)
	fmt.Fprintf(w, "repeat_sha_match=%.2f%%\n", r.RepeatMatch)
	fmt.Fprintf(w, "strict_rejected=%v\n", r.StrictRejected)
	fmt.Fprintf(w, "loudness_dbfs=%.2f\n", r.LoudnessDBFS)
	fmt.Fprintf(w, "peak_dbfs=%.2f\n", r.PeakDBFS)
	fmt.Fprintf(w, "gap_fill_events=%d missing_frames=%d duplicates=%d\n", r.Gaps, r.MissingFrames, r.Duplicates)
	fmt.Fprintf(w, "digest=%s\n", r.Digest)
	if r.RepeatEnabled {
		fmt.Fprintf(w, "repeat_digest=%s\n", r.RepeatDigest)
	}
	fmt.Fprintf(w, "out=%s\n", r.OutPath)
	fmt.Fprintf(w, "elapsed=%s\n", r.Elapsed.Round(time.Millisecond))
	if r.Passed() {
		fmt.Fprintln(w, "result=PASS")
	} else {
		fmt.Fprintln(w, "result=FAIL")
	}
}
