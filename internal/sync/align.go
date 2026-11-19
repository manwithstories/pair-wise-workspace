// Package sync aligns the audio and video timelines on one 90 kHz clock.
//
// Two distinct quantities are modeled deliberately:
//
//   - Media timestamps (PTS) are exact. Clock drift between the audio and
//     video encoders shows up only here, so drift estimation uses PTS only.
//   - Presentation times add network/playout jitter on arrival. The residual
//     sync error a viewer sees is measured from presentation times against
//     the audio reference clock.
//
// Keeping the two apart means a 600 ppm drift is estimated as 600 ppm even
// under heavy jitter, while sync_error_ms honestly reflects jitter too.
package sync

import (
	"math"
	"sort"

	"avpipe/internal/clock"
	"avpipe/internal/fault"
	"avpipe/internal/rng"
)

// Config controls the alignment simulation.
type Config struct {
	Seed       uint64
	JitterMs   float64 // one-sided uniform playout jitter
	DriftPPM   float64 // video clock runs fast by this many ppm
	Recoveries int     // loss events that force a resync
	Window     int     // pairs used by the estimator; 0 means cumulative
	Warmup     int     // pairs observed before correction starts; 0 means auto
}

// Pair is one audio/video timestamp pair. AudioPTS and VideoPTS are exact
// media timestamps; AudioPres and VideoPres additionally carry playout
// jitter.
type Pair struct {
	Tick      clock.Tick `json:"tick"`
	AudioPTS  clock.Tick `json:"audio_pts"`
	VideoPTS  clock.Tick `json:"video_pts"`
	AudioPres clock.Tick `json:"audio_pres"`
	VideoPres clock.Tick `json:"video_pres"`
	Error     float64    `json:"error_ms"`
	AfterFix  bool       `json:"after_fix,omitempty"`
}

// Result summarizes synchronization quality.
type Result struct {
	MedianMs        float64 `json:"sync_median_ms"`
	P95Ms           float64 `json:"sync_p95_ms"`
	MaxMs           float64 `json:"sync_max_ms"`
	P95BeforeMs     float64 `json:"sync_p95_before_ms"`
	MaxBeforeMs     float64 `json:"sync_max_before_ms"`
	DriftEstPPM     float64 `json:"drift_estimate_ppm"`
	DriftResolvable bool    `json:"drift_resolvable"`
	StdErrPPM       float64 `json:"drift_stderr_ppm"`
	Resynced        int     `json:"resyncs"`
	Pairs           []Pair  `json:"-"`
}

// slope estimates the video/audio clock-rate ratio by least squares on the
// exact PTS and returns the drift in ppm with its standard error. Jitter
// never enters this computation, so the estimate isolates clock drift.
func slope(pairs []Pair) (ppm, stderrPPM float64) {
	if len(pairs) < 3 {
		return 0, math.Inf(1)
	}
	n := float64(len(pairs))
	var meanX, meanY float64
	for _, p := range pairs {
		meanX += float64(p.AudioPTS)
		meanY += float64(p.VideoPTS)
	}
	meanX /= n
	meanY /= n
	var sxx, sxy float64
	for _, p := range pairs {
		dx := float64(p.AudioPTS) - meanX
		sxx += dx * dx
		sxy += dx * (float64(p.VideoPTS) - meanY)
	}
	if sxx == 0 {
		return 0, math.Inf(1)
	}
	ratio := sxy / sxx
	ppm = (ratio - 1) * 1e6
	var sse float64
	for _, p := range pairs {
		dx := float64(p.AudioPTS) - meanX
		resid := float64(p.VideoPTS) - meanY - ratio*dx
		sse += resid * resid
	}
	df := n - 2
	if df <= 0 || sse == 0 {
		return ppm, 0
	}
	se := math.Sqrt(sse/df) / math.Sqrt(sxx)
	return ppm, se * 1e6
}

// EstimateDrift recovers the rate difference from timestamp pairs in ppm.
func EstimateDrift(pairs []Pair) float64 {
	est, _ := slope(pairs)
	return est
}

// Align generates pairs on a fixed grid, injects drift and jitter, then
// corrects each video presentation time by the drift estimate from the
// preceding window. Drift is only reported when it is resolvable from the
// noise: a short run with no real drift reports 0, not a random slope.
func Align(cfg Config, intervals int, step clock.Tick) (Result, error) {
	if intervals <= 0 || step <= 0 {
		return Result{}, fault.New(fault.CodeInvalidArgument, "sync.align",
			"need positive intervals and step, got %d x %d", intervals, step)
	}
	if cfg.JitterMs < 0 {
		return Result{}, fault.New(fault.CodeInvalidArgument, "sync.align",
			"jitter %v ms is negative", cfg.JitterMs)
	}
	window := cfg.Window
	if window <= 0 {
		window = intervals
	}
	warmup := cfg.Warmup
	if warmup <= 0 {
		warmup = intervals / 8
		if warmup < 16 {
			warmup = 16
		}
		if warmup > 128 {
			warmup = 128
		}
	}
	r := rng.New(cfg.Seed)
	recoveryAt := make(map[int]bool, cfg.Recoveries)
	for i := 0; i < cfg.Recoveries; i++ {
		recoveryAt[r.Intn(intervals)] = true
	}

	raw := make([]Pair, 0, intervals)
	var before []float64
	for i := 0; i < intervals; i++ {
		trueTick := step * clock.Tick(i+1)
		drift := clock.Tick(float64(trueTick) * cfg.DriftPPM / 1e6)
		audioJitter := clock.FromMillis(r.Float() * cfg.JitterMs)
		videoJitter := clock.FromMillis(r.Float() * cfg.JitterMs)
		p := Pair{Tick: trueTick,
			AudioPTS:  trueTick,
			VideoPTS:  trueTick + drift,
			AudioPres: trueTick + audioJitter,
			VideoPres: trueTick + drift + videoJitter,
		}
		p.Error = (p.VideoPres - p.AudioPres).Millis()
		before = append(before, math.Abs(p.Error))
		raw = append(raw, p)
	}

	res := Result{Pairs: make([]Pair, 0, intervals)}
	var after []float64
	for i, p := range raw {
		est := 0.0
		if i >= warmup && i > 0 {
			from := i - window
			if from < 0 {
				from = 0
			}
			est, _ = slope(raw[from : i+1])
		}
		corrected := p.VideoPres - clock.Tick(float64(p.VideoPres)*est/1e6)
		np := p
		np.VideoPres = corrected
		np.Error = (corrected - p.AudioPres).Millis()
		if recoveryAt[i] {
			res.Resynced++
			np.AfterFix = true
		}
		after = append(after, math.Abs(np.Error))
		res.Pairs = append(res.Pairs, np)
	}
	est, se := slope(raw)
	res.StdErrPPM = se
	res.DriftResolvable = math.IsInf(se, 1) || math.Abs(est) > 2*se
	res.DriftEstPPM = est
	if !res.DriftResolvable {
		res.DriftEstPPM = 0
	}
	sort.Float64s(before)
	sort.Float64s(after)
	res.P95BeforeMs = clock.Percentile(before, 0.95)
	res.MaxBeforeMs = before[len(before)-1]
	res.MedianMs = clock.Percentile(after, 0.5)
	res.P95Ms = clock.Percentile(after, 0.95)
	res.MaxMs = after[len(after)-1]
	return res, nil
}

// ApplyResync corrects video presentation times by driftPPM and returns the
// corrected slice plus how many pairs changed.
func ApplyResync(pairs []Pair, driftPPM float64) ([]Pair, int) {
	out := make([]Pair, len(pairs))
	fixed := 0
	for i, p := range pairs {
		correction := clock.Tick(float64(p.VideoPres) * driftPPM / 1e6)
		np := p
		np.VideoPres = p.VideoPres - correction
		np.Error = (np.VideoPres - p.AudioPres).Millis()
		if correction != 0 {
			fixed++
		}
		out[i] = np
	}
	return out, fixed
}

// JitterStats returns the playout jitter spread in milliseconds.
func JitterStats(pairs []Pair) (meanMs, maxMs float64) {
	if len(pairs) == 0 {
		return 0, 0
	}
	var acc float64
	for _, p := range pairs {
		d := math.Abs((p.AudioPres - p.AudioPTS).Millis())
		acc += d
		if d > maxMs {
			maxMs = d
		}
	}
	return acc / float64(len(pairs)), maxMs
}
