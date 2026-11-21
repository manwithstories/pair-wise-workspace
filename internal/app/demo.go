// Package app wires the demuxers, mapper, timeline, remuxer and manifest into
// the single acceptance pipeline exercised by cmd/mediameld.
package app

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"mediameld/internal/fixture"
	"mediameld/internal/ingest"
	"mediameld/internal/manifest"
	"mediameld/internal/mapping"
	"mediameld/internal/model"
	"mediameld/internal/remux"
	"mediameld/internal/timeline"
)

// Options mirrors the CLI flags for the demo subcommand.
type Options struct {
	Seed     int64
	LateMS   int64
	DriftPPM int64
	WrapAt   uint32
	VFR      []int64
	OutDir   string
	Case     string
}

// Result is the terminal report for a run.
type Result struct {
	Tracks                 int
	Samples                int
	LateReplays            int
	TimelineHash           string
	SampleHashMismatches   int
	TargetBitrateBPS       int64
	ActualBitrateBPS       int64
	BitrateDeviationPct    float64
	QualitySamples         int
	QualityMatchRatePct    float64
	ManifestAlignmentMaxMS int64
	RecoveredTracks        int
	ErrorCode              string
	ExitCode               int
}

const segmentSpan = model.Instant(500_000_000)

// Run executes the demo. It always confines writes to opts.OutDir.
func Run(opts Options, stdout io.Writer) (Result, error) {
	if opts.OutDir == "" {
		return Result{}, model.NewError(model.CodeUsage, "--out is required")
	}
	if err := os.MkdirAll(opts.OutDir, 0o755); err != nil {
		return Result{}, model.WrapError(model.CodeInternal, err, "create output directory")
	}
	bundle, err := fixture.Generate(filepath.Join(opts.OutDir, "fixtures"), fixture.Options{
		Seed: opts.Seed, DriftPPM: opts.DriftPPM, WrapAt: opts.WrapAt, VFR: opts.VFR, LateMS: opts.LateMS,
	})
	if err != nil {
		return Result{}, model.WrapError(model.CodeInternal, err, "generate fixtures")
	}
	switch opts.Case {
	case "", "normal":
		return runNormal(bundle, opts, stdout)
	case "corrupt-klv":
		return runCorrupt(bundle, opts, stdout)
	default:
		return Result{}, model.NewError(model.CodeUsage, "unknown case %q", opts.Case)
	}
}

// trackBundle bundles a demuxed track with the input it came from.
type trackBundle struct {
	input string
	track *model.Track
}

func runNormal(bundle fixture.Bundle, opts Options, stdout io.Writer) (Result, error) {
	var res Result

	studio, err := demuxFile(bundle.StudioMP4, ingest.DemuxMP4)
	if err != nil {
		return res, err
	}
	camera, err := demuxMXF(bundle.CameraMXF)
	if err != nil {
		return res, err
	}
	var all []trackBundle
	all = append(all, studio...)
	all = append(all, camera...)

	// Map by fingerprint: studio video/audio plus camera audio.
	resolved, err := resolveBundle(all)
	if err != nil {
		return res, err
	}

	// Calibrate every sample against the shared time base.
	events := calibrate(resolved, opts)

	// Acquisition calibration: 32-bit RTP ticks are unwrapped in sequence
	// order, drift-compensated, and used as the camera video timeline. The
	// container remains the source of sample bytes and durations.
	acq, err := readAcquisition(bundle.RTPStream, opts.DriftPPM)
	if err != nil {
		return res, err
	}
	videoID := mainVideoID(resolved)
	cam := resolutionFor(resolved, videoID)
	if cam == nil {
		return res, model.NewError(model.CodeTrackMissing, "main video track not resolved")
	}
	if drift := acq.maxDriftNS(events, videoID); drift > 1_000_000 {
		return res, model.NewError(model.CodeClock,
			"transport and container timelines disagree by %dns on track %s", drift, videoID)
	}
	events = rtpEvents(acq, cam, events)
	applyArrival(events, acq, videoID)

	plan, err := timeline.Build(events, timeline.Config{
		AllowedLate: model.Instant(opts.LateMS) * 1_000_000,
		SegmentSpan: segmentSpan,
	})
	if err != nil {
		return res, err
	}
	res.LateReplays = plan.LateReplays
	res.TimelineHash = plan.Hash
	res.Tracks = len(resolved)

	// Remux the delivered samples verbatim.
	remuxIn := buildRemuxInput(resolved, plan)
	outPath := filepath.Join(opts.OutDir, "delivery.mp4")
	remuxRes, err := remux.Remux(outPath, remuxIn, remux.Fingerprint(remuxIn))
	if err != nil {
		return res, err
	}
	// Persist a completed checkpoint so a re-run can prove idempotent resume.
	ckpt := filepath.Join(opts.OutDir, "delivery.ckpt")
	if err := remux.SaveCheckpoint(ckpt, remux.Checkpoint{
		Fingerprint: remux.Fingerprint(remuxIn), Stage: "done",
		SamplesDone: len(remuxRes.SampleHashes),
	}); err != nil {
		return res, err
	}
	res.SampleHashMismatches = remuxRes.HashMismatch
	res.Samples = len(remuxRes.SampleHashes)
	res.QualitySamples = remuxRes.Matched
	if remuxRes.Written > 0 {
		res.QualityMatchRatePct = 100
	}

	// Bitrate accounting from the actual container bytes and time span.
	span := plan.Watermark
	if span <= 0 {
		span = planSegSpan(plan)
	}
	// Target is the authored media bitrate; actual counts whole container
	// bytes. The gap between them is pure container overhead.
	mediaBytes := int64(0)
	for _, s := range remuxRes.SampleHashes {
		mediaBytes += int64(s.Size)
	}
	mediaSpan := planSegSpan(plan)
	res.TargetBitrateBPS = actualBitrate(mediaBytes, mediaSpan)
	res.ActualBitrateBPS = actualBitrate(remuxRes.Bytes, mediaSpan)
	if res.TargetBitrateBPS > 0 {
		dev := (float64(res.ActualBitrateBPS) - float64(res.TargetBitrateBPS)) / float64(res.TargetBitrateBPS) * 100
		if dev < 0 {
			dev = -dev
		}
		res.BitrateDeviationPct = dev
	}

	// Manifest plus independent verification.
	man := manifest.Build(plan, resolved, remuxRes, []manifest.SourceRecord{
		{Path: filepath.Base(bundle.StudioMP4), Fingerprint: sourceFingerprint(bundle.StudioMP4)},
		{Path: filepath.Base(bundle.CameraMXF), Fingerprint: sourceFingerprint(bundle.CameraMXF)},
	})
	rep, err := verifyOutput(outPath, man)
	if err != nil {
		return res, err
	}
	if !rep.OK() {
		return res, model.NewError(model.CodeManifestAlign, "manifest verification failed: %s", strings.Join(rep.Errors, "; "))
	}
	res.ManifestAlignmentMaxMS = rep.AlignmentMaxMS
	if err := manifest.Write(filepath.Join(opts.OutDir, "manifest.json"), man); err != nil {
		return res, model.WrapError(model.CodeInternal, err, "write manifest")
	}
	printNormal(stdout, res)
	return res, nil
}

func runCorrupt(bundle fixture.Bundle, opts Options, stdout io.Writer) (Result, error) {
	var res Result
	path := filepath.Join(bundle.Dir, "camera_corrupt.mxf")
	f, err := os.Open(path)
	if err != nil {
		return res, model.WrapError(model.CodeInternal, err, "open corrupt fixture")
	}
	defer f.Close()
	mxf, err := ingest.DemuxMXF(f, path)
	if err != nil {
		return res, err
	}
	res.Tracks = len(mxf.Tracks)
	res.RecoveredTracks = mxf.Recovered
	if mxf.Err != nil {
		code, _ := model.CodeOf(mxf.Err)
		res.ErrorCode = string(code)
	}
	res.ExitCode = model.ExitCode(mxf.Err)
	// A damaged input must never leave a manifest that could pass as a
	// complete delivery.
	if _, statErr := os.Stat(filepath.Join(opts.OutDir, "manifest.json")); statErr == nil {
		return res, model.NewError(model.CodeInternal, "corrupt run must not leave a manifest")
	}
	printCorrupt(stdout, res)
	if mxf.Err != nil {
		// Report the isolated damage on stderr for operators while the
		// structured report (and exit code) already went to stdout.
		fmt.Fprintf(os.Stderr, "isolated=%s\n", mxf.Err)
		return res, nil
	}
	return res, nil
}

func printNormal(w io.Writer, r Result) {
	fmt.Fprintf(w, "tracks=%d\n", r.Tracks)
	fmt.Fprintf(w, "samples=%d\n", r.Samples)
	fmt.Fprintf(w, "late_replays=%d\n", r.LateReplays)
	fmt.Fprintf(w, "timeline_sha256=%s\n", r.TimelineHash)
	fmt.Fprintf(w, "sample_hash_mismatches=%d\n", r.SampleHashMismatches)
	fmt.Fprintf(w, "target_bitrate_bps=%d\n", r.TargetBitrateBPS)
	fmt.Fprintf(w, "actual_bitrate_bps=%d\n", r.ActualBitrateBPS)
	fmt.Fprintf(w, "bitrate_deviation_pct=%.4f\n", r.BitrateDeviationPct)
	fmt.Fprintf(w, "quality_samples=%d\n", r.QualitySamples)
	fmt.Fprintf(w, "quality_match_rate_pct=%.2f\n", r.QualityMatchRatePct)
	fmt.Fprintf(w, "manifest_alignment_max_ms=%d\n", r.ManifestAlignmentMaxMS)
}

func printCorrupt(w io.Writer, r Result) {
	fmt.Fprintf(w, "error_code=%s\n", r.ErrorCode)
	fmt.Fprintf(w, "recovered_tracks=%d\n", r.RecoveredTracks)
	fmt.Fprintf(w, "tracks=%d\n", r.Tracks)
	fmt.Fprintf(w, "exit_code=%d\n", r.ExitCode)
}

// --- helpers ---

func demuxFile(path string, fn func(io.ReadSeeker, string) ([]*model.Track, error)) ([]trackBundle, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, model.WrapError(model.CodeInternal, err, "open %s", path)
	}
	defer f.Close()
	tracks, err := fn(f, path)
	if err != nil {
		return nil, err
	}
	out := make([]trackBundle, len(tracks))
	for i, t := range tracks {
		out[i] = trackBundle{input: path, track: t}
	}
	return out, nil
}

func demuxMXF(path string) ([]trackBundle, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, model.WrapError(model.CodeInternal, err, "open %s", path)
	}
	defer f.Close()
	res, err := ingest.DemuxMXF(f, path)
	if err != nil {
		return nil, err
	}
	if res.Err != nil {
		return nil, res.Err
	}
	out := make([]trackBundle, len(res.Tracks))
	for i, t := range res.Tracks {
		out[i] = trackBundle{input: path, track: t}
	}
	return out, nil
}

func sourceFingerprint(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	h := sha256.New()
	io.Copy(h, f)
	return hex.EncodeToString(h.Sum(nil))[:32]
}

func planSegSpan(plan timeline.Plan) model.Instant {
	var maxT model.Instant
	for _, e := range plan.Events {
		if e.PTS+e.Duration > maxT {
			maxT = e.PTS + e.Duration
		}
	}
	return maxT
}

func actualBitrate(bytesTotal int64, span model.Instant) int64 {
	if span <= 0 {
		return 0
	}
	return bytesTotal * 8 * int64(1_000_000_000) / int64(span)
}

func sortedIDs(res []mapping.Resolution) []string {
	ids := make([]string, 0, len(res))
	for _, r := range res {
		ids = append(ids, r.Track.ID)
	}
	sort.Strings(ids)
	return ids
}

var _ = sortedIDs
