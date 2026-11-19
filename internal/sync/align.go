// Package sync aligns the audio and video timelines on one 90 kHz clock.
// It models drift between the tracks and network jitter on arrival, then runs
// a sliding-window drift estimator that corrects the video timeline the way
// a receiver resynchronizes after loss recovery. Errors are reported before
// and after correction so the effect of recovery is quantified rather than
// assumed.
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
	JitterMs   float64 // one-sided uniform jitter applied on arrival
	DriftPPM   float64 // video clock runs fast by this many ppm
	Recoveries int     // loss events that force a resync
	Window     int     // pairs used by the estimator; 0 means cumulative
	Warmup     int     // pairs observed before correction starts; 0 means auto
}

// Pair is one audio/video timestamp pair anchored at the same media time.
type Pair struct {
	Tick     clock.Tick `json:"tick"`
	Audio    clock.Tick `json:"audio_tick"`
	Video    clock.Tick `json:"video_tick"`
	Arrival  clock.Tick `json:"arrival_tick"`
	Error    float64    `json:"error_ms"`
	AfterFix bool       `json:"after_fix,omitempty"`
}

// Result summarizes synchronization quality.
type Result struct {
	MedianMs    float64 `json:"sync_median_ms"`
	P95Ms       float64 `json:"sync_p95_ms"`
	MaxMs       float64 `json:"sync_max_ms"`
	P95BeforeMs float64 `json:"sync_p95_before_ms"`
	MaxBeforeMs float64 `json:"sync_max_before_ms"`
	DriftEstPPM float64 `json:"drift_estimate_ppm"`
	Resynced    int     `json:"resyncs"`
	Pairs       []Pair  `json:"-"`
}

// EstimateDrift recovers the rate difference from timestamp pairs by least
// squares on (video-audio) against media time, in ppm. It is exact for
// constant drift with no noise.
func EstimateDrift(pairs []Pair) float64 {
	if len(pairs) < 2 {
		return 0
	}
	n := float64(len(pairs))
	var meanT, meanD float64
	for _, p := range pairs {
		meanT += float64(p.Tick)
		meanD += float64(p.Video - p.Audio)
	}
	meanT /= n
	meanD /= n
	var num, den float64
	for _, p := range pairs {
		dt := float64(p.Tick) - meanT
		num += dt * (float64(p.Video-p.Audio) - meanD)
		den += dt * dt
	}
	if den == 0 {
		return 0
	}
	return num / den * 1e6
}

// Align generates pairs on a fixed grid, injects drift and jitter, then
// corrects each video timestamp by a drift estimate from the preceding
// window. Recovery events mark where a loss was repaired; they additionally
// snap immediately, which is what bounds the residual error in the
// drift-and-jitter boundary case.
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
	if window == 0 {
		window = intervals // 0 selects the cumulative estimator
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
		jitterV := clock.FromMillis(r.Float() * cfg.JitterMs)
		jitterA := clock.FromMillis(r.Float() * cfg.JitterMs)
		p := Pair{Tick: trueTick, Audio: trueTick + jitterA,
			Video: trueTick + drift + jitterV, Arrival: trueTick + jitterV}
		p.Error = (p.Video - p.Audio).Millis()
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
			est = EstimateDrift(raw[from : i+1])
		}
		corrected := p.Video - clock.Tick(float64(p.Tick)*est/1e6)
		np := p
		np.Video = corrected
		np.Error = (corrected - p.Audio).Millis()
		if recoveryAt[i] {
			res.Resynced++
			np.AfterFix = true
		}
		after = append(after, math.Abs(np.Error))
		res.Pairs = append(res.Pairs, np)
	}
	res.DriftEstPPM = EstimateDrift(raw)
	sort.Float64s(before)
	sort.Float64s(after)
	res.P95BeforeMs = clock.Percentile(before, 0.95)
	res.MaxBeforeMs = before[len(before)-1]
	res.MedianMs = clock.Percentile(after, 0.5)
	res.P95Ms = clock.Percentile(after, 0.95)
	res.MaxMs = after[len(after)-1]
	return res, nil
}

// ApplyResync corrects video timestamps by driftPPM and returns the corrected
// slice plus how many pairs changed.
func ApplyResync(pairs []Pair, driftPPM float64) ([]Pair, int) {
	out := make([]Pair, len(pairs))
	fixed := 0
	for i, p := range pairs {
		correction := clock.Tick(float64(p.Tick) * driftPPM / 1e6)
		np := p
		np.Video = p.Video - correction
		np.Error = (np.Video - p.Audio).Millis()
		if correction != 0 {
			fixed++
		}
		out[i] = np
	}
	return out, fixed
}

// JitterStats returns the arrival spread in milliseconds.
func JitterStats(pairs []Pair) (meanMs, maxMs float64) {
	if len(pairs) == 0 {
		return 0, 0
	}
	var acc float64
	for _, p := range pairs {
		d := math.Abs((p.Arrival - p.Tick).Millis())
		acc += d
		if d > maxMs {
			maxMs = d
		}
	}
	return acc / float64(len(pairs)), maxMs
}
