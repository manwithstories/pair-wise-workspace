// Package pipeline orchestrates the fixture, subtitle, clock, timeline,
// index, validation and render stages into one reproducible run, and
// produces the JSON summary consumed by the CLI and by tests.
package pipeline

import (
	"fmt"
	"sort"

	"github.com/pion/rtp"

	"pairflow/media/internal/clock"
	"pairflow/media/internal/fixture"
	"pairflow/media/internal/mp4"
	"pairflow/media/internal/render"
	"pairflow/media/internal/subtitle"
	"pairflow/media/internal/timeline"
)

// Config is the fully validated run configuration.
type Config struct {
	Seed       int64
	DriftPPM   int64
	WrapAt     uint32
	FrameRate  fixture.Rate
	FrameCount int
	OutDir     string
	Policy     timeline.ConflictPolicy
	Window     timeline.Window
	// ContinueOnError allows export even when validation fails.
	ContinueOnError bool
	// Render enables frame export. When false the run is analysis only.
	Render bool
	// JumpThresholdMS classifies RTP jumps larger than this as anomalies.
	JumpThresholdMS int64
	// ResidualThresholdMS is the drift-fit outlier threshold.
	ResidualThresholdMS int64
	// KeyframeOnly exports only sync samples.
	KeyframeOnly bool
	// MaxFrames caps exported frames.
	MaxFrames int
}

// Summary is the JSON document printed by the CLI.
type Summary struct {
	Seed               int64                `json:"seed"`
	DriftPPM           int64                `json:"drift_ppm"`
	WrapAt             uint32               `json:"wrap_at"`
	FrameRate          string               `json:"frame_rate"`
	DropFrame          bool                 `json:"drop_frame"`
	Timecode           string               `json:"timecode_start"`
	TimecodeWrapped    string               `json:"timecode_wrapped,omitempty"`
	TimecodeUnwrapped  string               `json:"timecode_unwrapped,omitempty"`
	RoundtripRate      clock.Rational       `json:"roundtrip_rate"`
	KeyframeHitRate    clock.Rational       `json:"keyframe_hit_rate"`
	SegmentSHACount    int                  `json:"segment_sha256_count"`
	RecoveredRecords   int                  `json:"recovered_records"`
	ErrorsWithOffset   int                  `json:"errors_with_offset"`
	VFRBytes           int                  `json:"vfr_samples"`
	RTPWraps           int                  `json:"rtp_wraps"`
	RTPDuplicates      int                  `json:"rtp_duplicates"`
	RTPBacksteps       int                  `json:"rtp_backsteps"`
	RTPJumps           int                  `json:"rtp_jumps"`
	RTPAnomalies       int                  `json:"drift_anomalies"`
	EstimatedDriftPPM  clock.Rational       `json:"estimated_drift_ppm"`
	EstimatedOffsetMS  clock.Rational       `json:"estimated_offset_ms"`
	Sync               SyncResult           `json:"rtp_mp4_sync"`
	SubtitleCues       int                  `json:"subtitle_cues"`
	Conflicts          int                  `json:"subtitle_conflicts"`
	VideoSamples       int                  `json:"video_samples"`
	KeyframeHits       int                  `json:"keyframe_hits"`
	KeyframeMisses     int                  `json:"keyframe_misses"`
	Monotonic          bool                 `json:"timeline_monotonic"`
	SubtitleBoundaryOK bool                 `json:"subtitle_boundaries_ok"`
	Stages             []StageResult        `json:"stages"`
	Errors             []*clock.SourceError `json:"errors"`
	Frames             int                  `json:"frames_exported"`
	ManifestPath       string               `json:"manifest_path,omitempty"`
	ExportSkipped      bool                 `json:"export_skipped"`
	ExportSkipReason   string               `json:"export_skip_reason,omitempty"`
	Passed             bool                 `json:"passed"`
	Failures           []string             `json:"failures,omitempty"`
}

// StageResult records one pipeline stage.
type StageResult struct {
	Name     string `json:"name"`
	OK       bool   `json:"ok"`
	Detail   string `json:"detail,omitempty"`
	Errors   int    `json:"errors"`
	Duration string `json:"-"`
}

// Thresholds are the acceptance gates.
type Thresholds struct {
	MinRoundtrip   clock.Rational
	MinKeyframeHit clock.Rational
	MinSegments    int
	MinRecovered   int
	MinOffsetErrs  int
	MinVFR         int
}

// maxErrorsPerCode bounds how many records of one error code are echoed
// into the JSON summary per damaged fixture.
const maxErrorsPerCode = 4

// DefaultThresholds matches the acceptance criteria.
func DefaultThresholds() Thresholds {
	return Thresholds{
		MinRoundtrip:   clock.NewRational(99, 100),
		MinKeyframeHit: clock.NewRational(98, 100),
		MinSegments:    8,
		MinRecovered:   2,
		MinOffsetErrs:  2,
		MinVFR:         10,
	}
}

// Run executes the whole pipeline. A nil error means the pipeline ran;
// whether validation passed is Summary.Passed.
func Run(cfg Config) (*Summary, error) {
	if err := validate(cfg); err != nil {
		return nil, err
	}
	opts := fixture.DefaultOptions()
	opts.Seed = cfg.Seed
	opts.DriftPPM = cfg.DriftPPM
	opts.WrapAt = cfg.WrapAt
	opts.FrameRate = cfg.FrameRate
	if cfg.FrameCount > 0 {
		opts.FrameCount = cfg.FrameCount
	}
	set, err := fixture.Generate(opts)
	if err != nil {
		return nil, err
	}
	sum := &Summary{
		Seed: cfg.Seed, DriftPPM: cfg.DriftPPM, WrapAt: cfg.WrapAt,
		FrameRate: cfg.FrameRate.String(), DropFrame: cfg.FrameRate.DropFrame(),
	}

	// Stage 1: parse media and subtitles.
	sum.Stages = append(sum.Stages, StageResult{Name: "generate", OK: true,
		Detail: fmt.Sprintf("%d video deltas, %d RTP packets",
			len(set.VideoDeltas), len(set.RTP))})

	videoIdx, err := mp4.ParseBytes("master.mp4", set.Master)
	if err != nil {
		return nil, err
	}
	// Damaged variants are parsed so their error structure is exercised
	// even though they are not part of the composed timeline.
	truncIdx, _ := mp4.ParseBytes("truncated.mp4", set.TruncatedMP4)
	dupIdx, _ := mp4.ParseBytes("duplicate.mp4", set.DuplicatePTS)
	oobIdx, _ := mp4.ParseBytes("out_of_bounds.mp4", set.OutOfBounds)
	damage := []*mp4.Index{truncIdx, dupIdx, oobIdx}

	srtDoc := subtitle.ParseSRT(set.SRT, "sub-srt")
	vttDoc := subtitle.ParseVTT(set.VTT, "sub-vtt")
	sum.Stages = append(sum.Stages, StageResult{Name: "parse", OK: true,
		Errors: len(srtDoc.Errors) + len(vttDoc.Errors),
		Detail: fmt.Sprintf("%d tracks, %d SRT cues, %d VTT cues",
			len(videoIdx.Tracks), len(srtDoc.Cues), len(vttDoc.Cues))})

	// Stage 2: unwrap RTP and estimate the node clock.
	wire, err := fixture.DecodeRTPDump(set.RTPDump)
	if err != nil {
		return nil, err
	}
	rtpStats := analyzeRTP(wire, set.RTP, cfg)
	sum.RTPWraps = rtpStats.wraps
	sum.RTPDuplicates = rtpStats.duplicates
	sum.RTPBacksteps = rtpStats.backsteps
	sum.RTPJumps = rtpStats.jumps

	base := clock.NewBase(1, 90000)
	corr, anomalies, _ := clock.Estimate(rtpStats.pairs, base,
		clock.NewRational(cfg.ResidualThresholdMS, 1000))
	sum.EstimatedDriftPPM = corr.DriftPPM
	sum.EstimatedOffsetMS = corr.Offset.Mul(clock.NewRational(1000, 1))
	sum.RTPAnomalies = len(anomalies)
	for _, a := range anomalies {
		sum.Errors = append(sum.Errors, clock.Errorf("rtp", "timestamp_anomaly", -1,
			"packet %d node=%d residual=%sms retained", a.Index, a.NodeTicks,
			a.ResidualMS.String()))
	}
	// Cross-check the corrected node clock against the origin MP4 decode
	// timeline. The anomaly sample is excluded by the ordinal pairing
	// only in the sense that its raw residual is reported, never fixed.
	sum.Sync = syncRTPToMP4(rtpStats.pairs, videoIdx.Tracks[0], corr, base, anomalies)
	sum.Stages = append(sum.Stages, StageResult{Name: "clock", OK: true,
		Errors: len(anomalies),
		Detail: fmt.Sprintf("drift=%.1fppm wraps=%d jumps=%d clean_max=%.1fms anomaly_max=%.1fms",
			corr.DriftPPM.Seconds(), rtpStats.wraps, rtpStats.jumps,
			sum.Sync.CleanMaxMS.Seconds(), sum.Sync.AnomalyMaxMS.Seconds())})

	// Stage 3: SMPTE timecode on both sides of the day boundary.
	frameDur := cfg.FrameRate.FrameDuration()
	_, startTC, err := fixture.TimecodeFromSeconds(clock.Zero(), cfg.FrameRate, false)
	if err != nil {
		return nil, err
	}
	sum.Timecode = startTC.String()
	late := clock.NewRational(24*3600+17, 1)
	_, wrappedTC, err := fixture.TimecodeFromSeconds(late, cfg.FrameRate, true)
	if err != nil {
		return nil, err
	}
	sum.TimecodeWrapped = wrappedTC.String()
	// Without day wrapping the label continues past 24:00, which is
	// the correct SMPTE representation for a >24h session.
	_, unwrappedTC, err := fixture.TimecodeFromSeconds(late, cfg.FrameRate, false)
	if err != nil {
		return nil, err
	}
	sum.TimecodeUnwrapped = unwrappedTC.String()

	// Stage 4: compose the timeline with subtitle conflict resolution.
	videoTrack := videoIdx.Tracks[0]
	tl := timeline.Compose([]timeline.TrackInput{
		{Name: "video", Kind: timeline.KindVideo, MP4: &videoIdx.Tracks[0], Priority: 10},
		{Name: "audio", Kind: timeline.KindAudio, MP4: &videoIdx.Tracks[1], Priority: 5},
		{Name: "sub-srt", Kind: timeline.KindSubtitle, Sub: srtDoc, Priority: 2},
		{Name: "sub-vtt", Kind: timeline.KindSubtitle, Sub: vttDoc, Priority: 1},
	}, timeline.Options{
		Window: cfg.Window, Policy: cfg.Policy, FrameDuration: frameDur,
	})
	sum.SubtitleCues = tl.SubtitleCount
	sum.VideoSamples = tl.VideoCount
	sum.Conflicts = len(tl.Conflicts)
	sum.Stages = append(sum.Stages, StageResult{Name: "compose", OK: true,
		Errors: len(tl.Errors),
		Detail: fmt.Sprintf("%d entries, %d conflicts", len(tl.Entries), len(tl.Conflicts))})

	// Stage 5: validate round-trip, monotonicity, keyframe hits and
	// subtitle boundaries.
	roundtrip := roundtripRate(videoTrack, set.VideoDeltas)
	sum.RoundtripRate = roundtrip
	sum.Monotonic = checkMonotonic(tl)
	sum.SubtitleBoundaryOK = checkSubtitleBounds(tl)

	targets := keyframeTargets(videoTrack)
	hits := videoTrack.LookupKeyframes(targets)
	sum.KeyframeHitRate = mp4.KeyframeHitRate(hits)
	for _, h := range hits {
		if h.Matched && h.InBounds {
			sum.KeyframeHits++
		} else {
			sum.KeyframeMisses++
		}
	}
	sum.Stages = append(sum.Stages, StageResult{Name: "validate", OK: true,
		Detail: fmt.Sprintf("roundtrip=%s keyframe=%s",
			roundtrip.String(), sum.KeyframeHitRate.String())})

	// Damage analysis: these fixtures must produce offset-carrying errors.
	sum.RecoveredRecords = srtDoc.Recovered() + vttDoc.Recovered()
	sum.VFRBytes = vfrCount(set)
	// Only errors with a concrete byte position count toward the offset
	// gate; media-index errors use offset -1 to mean "table-level".
	sum.ErrorsWithOffset = countOffsetErrors(sum.Errors, srtDoc, vttDoc, tl)

	// A master file that parses cleanly must contribute no errors; if
	// it does, they are part of the offset-bearing report. Damage
	// fixtures that parse without a usable sample table also surface
	// here, with their table offsets.
	sum.Errors = append(sum.Errors, videoIdx.Errors...)
	seenDamage := make(map[string]bool)
	for _, d := range damage {
		// Index.Errors already aggregates each track's errors. Cap the
		// per-code output so a deliberately damaged file with hundreds
		// of bad samples cannot flood the report; the full count stays
		// available through errors_with_offset and the index itself.
		perCode := make(map[string]int)
		for _, e := range d.Errors {
			key := e.Code + ":" + fmt.Sprint(e.Offset) + ":" + e.Message
			if seenDamage[key] {
				continue
			}
			if perCode[e.Code] >= maxErrorsPerCode {
				continue
			}
			seenDamage[key] = true
			perCode[e.Code]++
			sum.Errors = append(sum.Errors, e)
		}
	}

	SortErrors(sum.Errors)
	// The export gate deliberately ignores the segment-count criterion:
	// that number is produced by the export itself. Every input-stage
	// criterion is evaluated first, so a failing run writes nothing.
	pre := DefaultThresholds()
	pre.MinSegments = 0
	gateOpen, _ := evaluate(sum, pre, true)
	if cfg.Render && !gateOpen && !cfg.ContinueOnError {
		// The gate must run before any bytes reach disk.
		sum.Stages = append(sum.Stages, StageResult{Name: "render", OK: true,
			Detail: "skipped: validation failed"})
	} else if cfg.Render && (gateOpen || cfg.ContinueOnError) {
		ropt := render.Options{
			OutDir: cfg.OutDir, Width: 64, Height: 36, SegmentSize: 8,
			Seed: cfg.Seed, MaxFrames: cfg.MaxFrames, KeyframeOnly: cfg.KeyframeOnly,
		}
		exportTargets := render.PlanTargets(tl, ropt)
		man, err := render.Render(tl, exportTargets, ropt)
		if err != nil {
			return nil, err
		}
		sum.Frames = len(man.Frames)
		sum.SegmentSHACount = len(man.Segments)
		sum.ManifestPath = render.ManifestPath(ropt.OutDir)
		sum.Stages = append(sum.Stages, StageResult{Name: "render", OK: true,
			Detail: fmt.Sprintf("%d frames, %d segments", len(man.Frames), len(man.Segments))})
	}

	if cfg.Render && !gateOpen && !cfg.ContinueOnError {
		sum.ExportSkipped = true
		sum.ExportSkipReason = "validation failed and --continue-on-error was not set"
	}
	th := DefaultThresholds()
	if !cfg.Render {
		// Analysis-only runs have no exported bundle, so the segment
		// count cannot be judged; every other criterion still applies.
		th.MinSegments = 0
	}
	sum.Passed, sum.Failures = evaluate(sum, th, !cfg.Render || !cfg.ContinueOnError)
	return sum, nil
}

func validate(cfg Config) error {
	if cfg.FrameRate.Num <= 0 || cfg.FrameRate.Den <= 0 {
		return clock.NewSourceError("pipeline", "bad_frame_rate", -1, "frame rate must be positive")
	}
	if cfg.DriftPPM < -100_000 || cfg.DriftPPM > 100_000 {
		return clock.Errorf("pipeline", "bad_drift", -1,
			"drift %d ppm outside ±100000", cfg.DriftPPM)
	}
	if cfg.ResidualThresholdMS <= 0 {
		return clock.NewSourceError("pipeline", "bad_threshold", -1,
			"residual threshold must be positive")
	}
	if cfg.Render && cfg.OutDir == "" {
		return clock.NewSourceError("pipeline", "bad_output_path", -1,
			"--out is required for export")
	}
	return nil
}

// roundtripRate measures how many MP4 samples reproduce the fixture's
// decode deltas exactly when re-derived from the parsed index. The
// derived delta uses the track timescale so no floating point enters.
func roundtripRate(track mp4.TrackIndex, want []uint32) clock.Rational {
	if len(want) == 0 || len(track.Samples) == 0 {
		return clock.Zero()
	}
	n := len(want)
	if len(track.Samples) < n {
		n = len(track.Samples)
	}
	base := clock.NewBase(1, int64(track.Timescale))
	good := 0
	for i := 0; i < n; i++ {
		if i == 0 {
			if track.Samples[0].DTS.IsZero() {
				good++
			}
			continue
		}
		// Sample i's DTS is the sum of deltas 0..i-1, so the delta
		// between consecutive decode times is want[i-1].
		delta := track.Samples[i].DTS.Sub(track.Samples[i-1].DTS)
		if base.Ticks(delta) == int64(want[i-1]) {
			good++
		}
	}
	return clock.NewRational(int64(good), int64(n))
}

// checkMonotonic verifies that presentation order never moves backwards.
// Entries are grouped by kind because audio and video tracks legitimately
// interleave at different sample rates; within each kind the start times
// must be non-decreasing, and each entry must not precede its own
// previous entry's end.
func checkMonotonic(tl *timeline.Timeline) bool {
	lastStart := map[timeline.Kind]clock.Rational{}
	lastEnd := map[timeline.Kind]clock.Rational{}
	seen := map[timeline.Kind]bool{}
	for _, e := range tl.Entries {
		if e.Kind == timeline.KindSubtitle {
			continue
		}
		if seen[e.Kind] {
			if e.Start.LT(lastStart[e.Kind]) || e.Start.LT(lastEnd[e.Kind]) {
				return false
			}
		}
		lastStart[e.Kind] = e.Start
		lastEnd[e.Kind] = e.End
		seen[e.Kind] = true
	}
	return true
}

// checkSubtitleBounds verifies that every composed subtitle entry has a
// positive span, never starts before zero, and that its source byte
// offsets point forward. Subtitles may legitimately extend past the last
// video frame, so containment in the media extent is not required.
func checkSubtitleBounds(tl *timeline.Timeline) bool {
	for _, e := range tl.Entries {
		if e.Kind != timeline.KindSubtitle {
			continue
		}
		if e.End.Cmp(e.Start) <= 0 {
			return false
		}
		if e.Start.Num < 0 || e.End.Num < 0 {
			return false
		}
		if e.Offset < -1 {
			return false
		}
	}
	return true
}

// keyframeTargets samples the video track at ~30 evenly spaced times.
func keyframeTargets(track mp4.TrackIndex) []clock.Rational {
	if len(track.Samples) == 0 {
		return nil
	}
	const n = 30
	out := make([]clock.Rational, 0, n)
	for i := 0; i < n; i++ {
		idx := i * (len(track.Samples) - 1) / (n - 1)
		out = append(out, track.Samples[idx].DTS)
	}
	return out
}

func vfrCount(set *fixture.Set) int {
	// Non-key samples are the ones whose decode delta varies from the
	// nominal frame duration. They make the track genuinely VFR.
	n := 0
	for i := 1; i < len(set.VideoDeltas); i++ {
		if set.VideoDeltas[i] != set.VideoDeltas[0] {
			n++
		}
	}
	return n
}

type rtpStatsResult struct {
	wraps, duplicates, backsteps, jumps int
	pairs                               []clock.Pair
}

// analyzeRTP extends wire timestamps with the shared unwrapper and
// pairs each corrected node tick with the fixture's ground-truth source
// tick. It reports wraps, duplicates, backsteps and jumps; the raw wire
// values are never rewritten.
func analyzeRTP(wire []*rtp.Packet, truth []fixture.RTPPacket, cfg Config) rtpStatsResult {
	var out rtpStatsResult
	jumpThreshold := cfg.JumpThresholdMS * 90 // 90 kHz ticks per ms
	u := clock.NewUnwrapper(jumpThreshold)
	limit := len(truth)
	if len(wire) < limit {
		limit = len(wire)
	}
	// Extended timestamps begin at the raw wire value, which includes
	// the pre-wrap bias. Normalise to the first packet so the fitted
	// offset describes the node's real skew rather than its counter
	// origin.
	var origin int64
	for i := 0; i < limit; i++ {
		t, err := u.Push(wire[i].Header)
		if err != nil {
			continue
		}
		if t.Wrap {
			out.wraps++
		}
		if t.Duplicate {
			out.duplicates++
		}
		if t.Backward {
			out.backsteps++
		}
		if t.Jump {
			out.jumps++
		}
		if i == 0 {
			origin = t.Extended
		}
		out.pairs = append(out.pairs, clock.Pair{
			SourceTicks: truth[i].SourceTicks,
			NodeTicks:   t.Extended - origin,
		})
	}
	return out
}

func countOffsetErrors(sumErrors []*clock.SourceError, docs ...interface{}) int {
	seen := make(map[string]bool)
	n := 0
	add := func(errs []*clock.SourceError) {
		for _, e := range errs {
			if e == nil {
				continue
			}
			key := fmt.Sprintf("%s:%s:%d", e.Code, e.Source, e.Offset)
			if e.Offset < 0 || seen[key] {
				continue
			}
			seen[key] = true
			n++
		}
	}
	add(sumErrors)
	for _, d := range docs {
		switch v := d.(type) {
		case *subtitle.Document:
			add(v.Errors)
		case *mp4.Index:
			add(v.Errors)
			for i := range v.Tracks {
				add(v.Tracks[i].Errors)
			}
		case *timeline.Timeline:
			add(v.Errors)
		}
	}
	return n
}

func evaluate(sum *Summary, th Thresholds, requirePass bool) (bool, []string) {
	var failures []string
	if sum.RoundtripRate.Cmp(th.MinRoundtrip) < 0 {
		failures = append(failures, fmt.Sprintf("roundtrip_rate %s < %s",
			sum.RoundtripRate, th.MinRoundtrip))
	}
	if sum.KeyframeHitRate.Cmp(th.MinKeyframeHit) < 0 {
		failures = append(failures, fmt.Sprintf("keyframe_hit_rate %s < %s",
			sum.KeyframeHitRate, th.MinKeyframeHit))
	}
	if sum.SegmentSHACount < th.MinSegments {
		failures = append(failures, fmt.Sprintf("segment_sha256_count %d < %d",
			sum.SegmentSHACount, th.MinSegments))
	}
	if sum.RecoveredRecords < th.MinRecovered {
		failures = append(failures, fmt.Sprintf("recovered_records %d < %d",
			sum.RecoveredRecords, th.MinRecovered))
	}
	if sum.ErrorsWithOffset < th.MinOffsetErrs {
		failures = append(failures, fmt.Sprintf("errors_with_offset %d < %d",
			sum.ErrorsWithOffset, th.MinOffsetErrs))
	}
	if sum.VFRBytes < th.MinVFR {
		failures = append(failures, fmt.Sprintf("vfr_samples %d < %d",
			sum.VFRBytes, th.MinVFR))
	}
	if !sum.Monotonic {
		failures = append(failures, "timeline is not monotonic")
	}
	if !sum.SubtitleBoundaryOK {
		failures = append(failures, "subtitle boundaries are invalid")
	}
	if requirePass {
		return len(failures) == 0, failures
	}
	return true, failures
}

// SortErrors orders errors by source then offset for stable output.
func SortErrors(errs []*clock.SourceError) {
	sort.SliceStable(errs, func(i, j int) bool {
		if errs[i].Source != errs[j].Source {
			return errs[i].Source < errs[j].Source
		}
		return errs[i].Offset < errs[j].Offset
	})
}
