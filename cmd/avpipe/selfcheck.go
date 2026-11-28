package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"avpipe/internal/audit"
	"avpipe/internal/media"
)

// selfcheckConfig is the parameter set for a selfcheck run.
type selfcheckConfig struct {
	Dir      string
	Seed     int64
	Workers  int
	Queue    int
	Segment  media.Rational
	Cache    int
	WrapBits int
}

// selfcheckReport carries the metrics the acceptance contract prints.
type selfcheckReport struct {
	RoundtripRate     float64 `json:"roundtrip_rate"`
	KeyframeHitRate   float64 `json:"keyframe_hit_rate"`
	SegmentHashMatch  float64 `json:"segment_hash_match"`
	Segments          int     `json:"segments"`
	WrapRecovered     int     `json:"wrap_recovered"`
	VFRFrames         int     `json:"vfr_frames"`
	CorruptRejected   int     `json:"corrupt_rejected"`
	ReplayEqual       int     `json:"replay_equal"`
	QueuePeak         int     `json:"queue_peak"`
	CancelMS          int64   `json:"cancel_ms"`
	// CancelClean reports whether the canceled run left no published output
	// and no temporary files behind.
	CancelClean bool `json:"cancel_clean"`
	// Extra diagnostics, printed but not part of the graded metrics.
	GoodSegments      int      `json:"good_segments"`
	GoodPackets       int64    `json:"good_packets"`
	// DriftCuts counts segments the drift monitor annotated because an audio
	// clock had wandered past tolerance from the video reference clock. It is
	// zero for the well-formed fixture and positive for drift.mp4.
	DriftCuts  int      `json:"drift_cuts"`
	Problems   []string `json:"problems,omitempty"`
	Pass       bool     `json:"pass"`
}

// runSelfcheck drives the four required scenarios: normal conversion,
// corrupt-input rejection, cancellation, and a recovery re-run that must match
// the full run segment by segment.
func runSelfcheck(ctx context.Context, cfg selfcheckConfig) (*selfcheckReport, error) {
	if cfg.Dir == "" {
		return nil, media.Errorf(media.KindUsage, media.Origin{}, "selfcheck", "--dir is required")
	}
	if cfg.Seed == 0 {
		cfg.Seed = 17
	}
	if cfg.Workers <= 0 {
		cfg.Workers = 4
	}
	if cfg.Queue <= 0 {
		cfg.Queue = 8
	}
	if cfg.Cache <= 0 {
		cfg.Cache = cfg.Queue
	}
	if !cfg.Segment.Valid() {
		cfg.Segment = media.MustRat(2, 1)
	}
	if err := os.MkdirAll(cfg.Dir, 0o755); err != nil {
		return nil, media.New(media.KindUsage, media.Origin{File: cfg.Dir}, "selfcheck", "creating dir failed", err)
	}
	report := &selfcheckReport{}
	dir, err := filepath.Abs(cfg.Dir)
	if err != nil {
		return nil, media.New(media.KindUsage, media.Origin{File: cfg.Dir}, "selfcheck", "resolving dir failed", err)
	}

	// --- Generate fixtures. ---
	set, err := generateFixtures(filepath.Join(dir, "fixtures"), cfg.Seed)
	if err != nil {
		return nil, err
	}
	byName := map[string]string{}
	for _, f := range set.Files {
		byName[filepath.Base(f.Path)] = f.Path
	}
	good := byName["good.mp4"]
	if good == "" {
		return nil, media.Errorf(media.KindInternal, media.Origin{File: dir}, "selfcheck", "good.mp4 missing")
	}

	// --- Scenario 1: normal conversion. ---
	fullManifest := filepath.Join(dir, "manifest.json")
	fullOutput := filepath.Join(dir, "good.mp4")
	full, err := run(ctx, runConfig{
		Inputs: []string{good}, Output: fullOutput, Manifest: fullManifest, Format: "mp4",
		Segment: cfg.Segment, Workers: cfg.Workers, Queue: cfg.Queue, Cache: cfg.Cache,
		WrapBits: cfg.WrapBits, AllowQuarantine: true,
	})
	if err != nil {
		return nil, err
	}
	report.GoodSegments = full.Segments
	report.GoodPackets = full.Packets
	report.Segments = full.Segments
	report.QueuePeak = full.QueuePeak

	// Roundtrip: every output payload hash must appear in the source.
	report.RoundtripRate = roundtripRate(good, fullOutput)
	// Keyframe coverage: every segment begins on a keyframe.
	report.KeyframeHitRate = keyframeHitRate(fullManifest)
	// VFR: convert the VFR fixture and count irregular spacing.
	if vfrPath := byName["vfr.mp4"]; vfrPath != "" {
		vfrManifest := filepath.Join(dir, "vfr-manifest.json")
		vfrOut := filepath.Join(dir, "vfr-out.mp4")
		vres, verr := run(ctx, runConfig{
			Inputs: []string{vfrPath}, Output: vfrOut, Manifest: vfrManifest, Format: "mp4",
			Segment: cfg.Segment, Workers: cfg.Workers, Queue: cfg.Queue, Cache: cfg.Cache,
			WrapBits: 0, AllowQuarantine: true,
		})
		if verr != nil {
			return nil, verr
		}
		report.VFRFrames = countVFRFromManifest(vfrManifest)
		_ = vres
	}

	// --- Scenario 2: corrupt inputs must be rejected. ---
	// Four independent damage classes: a truncated MXF, a structurally broken
	// MP4 box, an out-of-bounds crop and an illegal rotation. Each must fail
	// with a corrupt verdict and must not publish an output file.
	corrupt := 0
	for _, name := range []string{"trunc.mxf", "trunc.mp4", "corrupt.mp4", "badcrop.mp4", "badrot.mp4"} {
		path := byName[name]
		if path == "" {
			continue
		}
		if rejected, _ := expectRejection(ctx, path, dir, cfg); rejected {
			corrupt++
		}
	}
	report.CorruptRejected = corrupt

	// --- Scenario 3: cancellation. ---
	report.CancelMS, report.CancelClean = measureCancel(ctx, byName["cancel.mp4"], dir, cfg)

	// --- Drift: a 120 ms audio offset must be detected and recorded. ---
	if driftPath := byName["drift.mp4"]; driftPath != "" {
		driftManifest := filepath.Join(dir, "drift-manifest.json")
		if _, derr := run(ctx, runConfig{
			Inputs: []string{driftPath}, Output: filepath.Join(dir, "drift-out.mp4"),
			Manifest: driftManifest, Format: "mp4",
			Segment: cfg.Segment, Workers: cfg.Workers, Queue: cfg.Queue, Cache: cfg.Cache,
			WrapBits: cfg.WrapBits, AllowQuarantine: true,
		}); derr != nil {
			return nil, derr
		}
		report.DriftCuts = countDriftReasons(driftManifest)
	}

	// --- Scenario 4: recovery re-run must match the full run. ---
	replayManifest := filepath.Join(dir, "manifest.replay.json")
	replayOutput := filepath.Join(dir, "good.replay.mp4")
	if _, err := run(ctx, runConfig{
		Inputs: []string{good}, Output: replayOutput, Manifest: replayManifest, Format: "mp4",
		Segment: cfg.Segment, Workers: cfg.Workers, Queue: cfg.Queue, Cache: cfg.Cache,
		WrapBits: cfg.WrapBits, AllowQuarantine: true,
	}); err != nil {
		return nil, err
	}
	// The published good.mp4 is the full-run output; the replay writes a
	// sibling. Compare the two manifests segment by segment.
	report.SegmentHashMatch = segmentHashMatch(fullManifest, replayManifest)
	if equal, _ := replayEqual(fullManifest, replayManifest); equal {
		report.ReplayEqual = 1
	}

	// --- Wrap recovery count from the wrap fixture. ---
	report.WrapRecovered = wrapRecoveryCount(ctx, byName["wrap.mp4"], dir, cfg)

	report.Pass = report.RoundtripRate >= 1.0 && report.KeyframeHitRate >= 1.0 &&
		report.SegmentHashMatch >= 1.0 && report.ReplayEqual == 1 &&
		report.CorruptRejected >= 4 && report.CancelMS <= 100 && report.CancelClean &&
		report.QueuePeak <= cfg.Queue && report.DriftCuts > 0 &&
		report.Segments == 6 && report.WrapRecovered == 2 && report.VFRFrames == 120
	if !report.Pass {
		report.Problems = describeShortfalls(report, cfg)
	}
	return report, nil
}

// printSelfcheck writes the acceptance line plus diagnostics.
func printSelfcheck(r *selfcheckReport) {
	fmt.Printf("roundtrip_rate=%.3f keyframe_hit_rate=%.3f segment_hash_match=%.3f segments=%d wrap_recovered=%d vfr_frames=%d corrupt_rejected=%d replay_equal=%d queue_peak=%d cancel_ms=%d\n",
		r.RoundtripRate, r.KeyframeHitRate, r.SegmentHashMatch, r.Segments, r.WrapRecovered,
		r.VFRFrames, r.CorruptRejected, r.ReplayEqual, r.QueuePeak, r.CancelMS)
	fmt.Printf("cancel_clean=%t drift_cuts=%d good_segments=%d good_packets=%d\n", r.CancelClean, r.DriftCuts, r.GoodSegments, r.GoodPackets)
	for _, p := range r.Problems {
		fmt.Printf("selfcheck shortfall: %s\n", p)
	}
	if r.Pass {
		fmt.Println("selfcheck: PASS")
	} else {
		fmt.Println("selfcheck: FAIL")
	}
}

// cmdSelfcheck parses flags and executes the selfcheck.
func cmdSelfcheck(ctx context.Context, args []string) error {
	fs := newSelfcheckFlagSet()
	if err := fs.Parse(args); err != nil {
		return media.New(media.KindUsage, media.Origin{}, "selfcheck", err.Error(), err)
	}
	dir := fs.Lookup("dir").Value.String()
	if dir == "" {
		return media.Errorf(media.KindUsage, media.Origin{}, "selfcheck", "--dir is required")
	}
	seg, err := media.ParseDuration(fs.Lookup("segment").Value.String())
	if err != nil {
		return media.New(media.KindUsage, media.Origin{}, "selfcheck", "invalid --segment", err)
	}
	queue := flagInt(fs, "queue")
	cache := flagInt(fs, "cache")
	if cache <= 0 {
		cache = queue
	}
	cfg := selfcheckConfig{
		Dir: dir, Seed: flagInt64(fs, "seed"), Workers: flagInt(fs, "workers"),
		Queue: queue, Segment: seg, Cache: cache, WrapBits: flagInt(fs, "wrap-bits"),
	}
	jsonOut := fs.Lookup("json").Value.String() == "true"
	report, err := runSelfcheck(ctx, cfg)
	if err != nil {
		return err
	}
	if jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(report); err != nil {
			return err
		}
	} else {
		printSelfcheck(report)
	}
	if !report.Pass {
		return media.Errorf(media.KindCorrupt, media.Origin{}, "selfcheck",
			"selfcheck failed: %s", strings.Join(report.Problems, "; "))
	}
	return nil
}

// verifyManifest is the terminal-facing wrapper around audit.Verify.
func verifyManifest(manifest, output string) (*audit.Verification, error) {
	return audit.Verify(manifest, output)
}
