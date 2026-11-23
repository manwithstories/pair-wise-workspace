// Package sync aligns audio sample time with video frame time and exposes
// every correction it applies. A correction beyond the configured threshold,
// or an interval without enough observations, halts that interval's commit.
package sync

import (
	"sort"

	"mediarun/internal/buffer"
	"mediarun/internal/clock"
)

// Config bounds the alignment.
type Config struct {
	// MaxCorrectionNS is the largest acceptable audio-to-video shift.
	MaxCorrectionNS int64
	// MinObservations is the minimum number of anchor pairs needed to fit an
	// alignment for one interval.
	MinObservations int
	// MinSpanNS is the minimum baseline between the first and last anchor.
	MinSpanNS int64
}

// DefaultConfig: 80ms of permitted shift (roughly two 40ms video frames),
// four anchor pairs and a 200ms baseline.
func DefaultConfig() Config {
	return Config{
		MaxCorrectionNS: 80_000_000,
		MinObservations: 4,
		MinSpanNS:       200_000_000,
	}
}

// Anchor pairs the time of one audio observation with the time of the video
// frame it belongs to, both on the unified ns axis.
type Anchor struct {
	AudioNS int64
	VideoNS int64
}

// Correction explains one alignment decision so provenance can replay it.
type Correction struct {
	StartNS int64
	EndNS   int64
	Applied int64 // ns added to audio to align with video
	Median  int64
	Points  int
	// Rejected is set when the fit succeeded but exceeded the threshold.
	Rejected bool
	Reason   string
}

// Aligner computes per-interval audio/video alignment.
type Aligner struct {
	cfg         Config
	track       string
	anchors     []Anchor
	corrections []Correction
}

// New creates an aligner for the audio track named track.
func New(track string, cfg Config) *Aligner {
	if cfg.MaxCorrectionNS <= 0 || cfg.MinObservations <= 0 {
		cfg = DefaultConfig()
	}
	return &Aligner{cfg: cfg, track: track}
}

// Add registers one anchor pair. Anchors are kept sorted by audio time.
func (a *Aligner) Add(an Anchor) {
	a.anchors = append(a.anchors, an)
	sort.SliceStable(a.anchors, func(i, j int) bool { return a.anchors[i].AudioNS < a.anchors[j].AudioNS })
}

// CorrectInterval computes the alignment for anchors inside [start, end].
// It returns KindUnrecoverable when there is not enough input to decide, and
// a threshold error when the fit succeeds but is too large to apply. In both
// cases the caller must stop committing that interval.
func (a *Aligner) CorrectInterval(start, end int64) (Correction, error) {
	var pts []Anchor
	for _, an := range a.anchors {
		if an.AudioNS >= start && an.AudioNS <= end {
			pts = append(pts, an)
		}
	}
	c := Correction{StartNS: start, EndNS: end, Points: len(pts)}
	if len(pts) < a.cfg.MinObservations {
		c.Reason = "too few anchors"
		a.corrections = append(a.corrections, c)
		return c, clock.New(clock.KindUnrecoverable, clock.CodeSyncInputShort,
			a.track, "not enough anchors to align interval")
	}
	span := pts[len(pts)-1].AudioNS - pts[0].AudioNS
	if span < a.cfg.MinSpanNS {
		c.Reason = "baseline too short"
		a.corrections = append(a.corrections, c)
		return c, clock.New(clock.KindUnrecoverable, clock.CodeSyncInputShort,
			a.track, "anchor baseline too short to align interval")
	}
	// The alignment is the median of per-anchor offsets: robust to one
	// mistimed anchor and cheap to explain.
	offsets := make([]int64, len(pts))
	for i, an := range pts {
		offsets[i] = an.VideoNS - an.AudioNS
	}
	sort.Slice(offsets, func(i, j int) bool { return offsets[i] < offsets[j] })
	c.Median = median(offsets)
	c.Applied = c.Median
	if c.Applied < -a.cfg.MaxCorrectionNS || c.Applied > a.cfg.MaxCorrectionNS {
		c.Rejected = true
		c.Reason = "correction beyond threshold"
		a.corrections = append(a.corrections, c)
		return c, clock.New(clock.KindUnrecoverable, clock.CodeSyncThreshold,
			a.track, "alignment exceeds permitted correction")
	}
	a.corrections = append(a.corrections, c)
	return c, nil
}

func median(sorted []int64) int64 {
	n := len(sorted)
	if n == 0 {
		return 0
	}
	if n%2 == 1 {
		return sorted[n/2]
	}
	return (sorted[n/2-1] + sorted[n/2]) / 2
}

// Corrections returns every decision made, applied or rejected.
func (a *Aligner) Corrections() []Correction { return a.corrections }

// ProbeDrift folds the anchors into clock.DriftPoints so the drift fitter can
// judge whether the audio clock is uniquely determined.
func (a *Aligner) ProbeDrift() []clock.DriftPoint {
	out := make([]clock.DriftPoint, len(a.anchors))
	for i, an := range a.anchors {
		out[i] = clock.DriftPoint{RefNS: an.VideoNS, MediaNS: an.AudioNS}
	}
	return out
}

// AlignIntervals walks committed buffer intervals pairwise and produces one
// correction per interval that has enough anchors. Intervals that fail are
// returned with their errors so the caller can stop those commits.
func AlignIntervals(a *Aligner, intervals []buffer.Interval) ([]Correction, []error) {
	var out []Correction
	var errs []error
	for _, iv := range intervals {
		c, err := a.CorrectInterval(iv.StartNS, iv.EndNS)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		out = append(out, c)
	}
	return out, errs
}
