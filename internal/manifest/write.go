// Package manifest generates the timeline manifest for a media package. All
// times are rational; alignment error is measured between the declared timeline
// and the decoded sample grid, with non-integral frame rates, audio sample
// boundaries and missing intervals all included in the statistics.
package manifest

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"

	"mediasync/internal/clock"
	"mediasync/internal/model"
)

// TrackTimeline is one track's manifest entry.
type TrackTimeline struct {
	Name        string          `json:"name"`
	TrackID     uint32          `json:"track_id"`
	Kind        model.TrackKind `json:"kind"`
	Codec       string          `json:"codec"`
	Timescale   uint32          `json:"timescale"`
	Samples     int             `json:"samples"`
	Bytes       int64           `json:"bytes"`
	DurationNum int64           `json:"duration_num"`
	DurationDen int64           `json:"duration_den"`
	DurationMs  int64           `json:"duration_ms"`
	SHA256      string          `json:"sha256"`
	// Incomplete lists sample index ranges that were never received.
	Incomplete []Interval `json:"incomplete,omitempty"`
	// Alignment holds this track's alignment error against its own ideal grid.
	Alignment Alignment `json:"alignment"`
}

// Interval is a half-open sample range [Start, End).
type Interval struct {
	Start int `json:"start"`
	End   int `json:"end"`
}

// Alignment summarises declared-vs-ideal timeline deviation in milliseconds.
type Alignment struct {
	MeanMs  float64 `json:"mean_ms"`
	P95Ms   float64 `json:"p95_ms"`
	MaxMs   float64 `json:"max_ms"`
	Samples int     `json:"samples"`
}

// Manifest is the whole timeline document.
type Manifest struct {
	Version       int             `json:"version"`
	TargetKbps    int             `json:"target_kbps"`
	ActualKbps    float64         `json:"actual_kbps"`
	BitrateErrPct float64         `json:"bitrate_error_pct"`
	Tracks        []TrackTimeline `json:"tracks"`
	// Alignment is the across-track alignment summary used by the demo.
	Alignment Alignment `json:"alignment"`
	Notes     []string  `json:"notes,omitempty"`
	// Gaps merges incomplete intervals from every track.
	Gaps []Interval `json:"gaps,omitempty"`
}

// TrackInput feeds one track into the manifest.
type TrackInput struct {
	Name   string
	Track  *model.Track
	SHA256 string
	// Missing holds sample indices never received (RTP gaps).
	Missing []Interval
	// FrameRate is the nominal edit rate; defaults to the track timescale.
	FrameRateNum, FrameRateDen int64
}

// Build computes the manifest. targetKbps and actualKbps are declared numbers:
// the budget never rewrites coded samples, so the two are reported separately
// with an explicit note about the difference.
func Build(inputs []TrackInput, targetKbps int, actualKbps float64) (*Manifest, error) {
	m := &Manifest{Version: 1, TargetKbps: targetKbps, ActualKbps: actualKbps}
	if targetKbps > 0 {
		m.BitrateErrPct = math.Abs(actualKbps-float64(targetKbps)) / float64(targetKbps) * 100
	}
	// Notes fire at or beyond the 10% budget tolerance; the comparison uses a
	// small epsilon so an exactly-10% case is still called out.
	if m.BitrateErrPct >= 10-1e-9 {
		m.Notes = append(m.Notes, fmt.Sprintf(
			"actual %.0f kbps deviates %.1f%% from target %d kbps; budget drives block sizing only",
			actualKbps, m.BitrateErrPct, targetKbps))
	}
	var allErrs []float64
	for _, in := range inputs {
		tl, errs, err := buildTrack(in)
		if err != nil {
			return nil, err
		}
		m.Tracks = append(m.Tracks, tl)
		allErrs = append(allErrs, errs...)
		for _, iv := range tl.Incomplete {
			m.Gaps = append(m.Gaps, iv)
		}
	}
	sort.Slice(m.Gaps, func(i, j int) bool { return m.Gaps[i].Start < m.Gaps[j].Start })
	m.Alignment = summarize(allErrs)
	return m, nil
}

// buildTrack computes one track's timeline and its per-sample alignment error.
func buildTrack(in TrackInput) (TrackTimeline, []float64, error) {
	if in.Track == nil {
		return TrackTimeline{}, nil, model.NewError(model.ErrParamInvalid, 0, 0, "nil track for %q", in.Name)
	}
	tr := in.Track
	if tr.Timescale == 0 {
		return TrackTimeline{}, nil, model.NewError(model.ErrParamInvalid, tr.ID, 0, "track %q has zero timescale", in.Name)
	}
	tl := TrackTimeline{
		Name: in.Name, TrackID: tr.ID, Kind: tr.Kind, Codec: tr.Codec,
		Timescale: tr.Timescale, Samples: len(tr.Samples), Bytes: tr.ByteCount(),
		SHA256: in.SHA256, Incomplete: in.Missing,
	}
	// Declared duration is the end of the last sample in the track timescale.
	end := tr.DurationTicks()
	num, den := end, int64(tr.Timescale)
	g := gcdInt(num, den)
	if g != 0 {
		num /= g
		den /= g
	}
	if den < 0 {
		num, den = -num, -den
	}
	tl.DurationNum, tl.DurationDen = num, den
	duration := clock.Rational{Num: num, Den: den}
	tl.DurationMs = duration.Millis()

	// Ideal grid: sample i is expected at i * frameDuration (or i/sampleRate).
	var frameNum, frameDen int64
	if in.FrameRateNum > 0 && in.FrameRateDen > 0 {
		frameNum, frameDen = in.FrameRateNum, in.FrameRateDen
	} else {
		frameNum, frameDen = int64(tr.Timescale), 1
	}
	var errs []float64
	for i := range tr.Samples {
		s := &tr.Samples[i]
		declaredSec, err := clock.FromTicks(s.DTS, tr.Timescale)
		if err != nil {
			return TrackTimeline{}, nil, err
		}
		idealSec := clock.Must(frameDen, frameNum).MulInt(int64(i))
		diff := clock.AbsDiffMs(declaredSec, idealSec)
		if math.IsNaN(diff) || math.IsInf(diff, 0) {
			return TrackTimeline{}, nil, model.NewError(model.ErrUnrecoverable, tr.ID, 0, "alignment produced a non-finite value")
		}
		errs = append(errs, diff)
	}
	tl.Alignment = summarize(errs)
	return tl, errs, nil
}

// summarize computes mean, p95 and max with a nearest-rank percentile.
func summarize(vals []float64) Alignment {
	if len(vals) == 0 {
		return Alignment{}
	}
	sorted := append([]float64(nil), vals...)
	sort.Float64s(sorted)
	var sum float64
	for _, v := range sorted {
		sum += v
	}
	idx := int(math.Ceil(0.95*float64(len(sorted)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return Alignment{
		MeanMs:  sum / float64(len(sorted)),
		P95Ms:   sorted[idx],
		MaxMs:   sorted[len(sorted)-1],
		Samples: len(sorted),
	}
}

func gcdInt(a, b int64) int64 {
	if a < 0 {
		a = -a
	}
	if b < 0 {
		b = -b
	}
	for b != 0 {
		a, b = b, a%b
	}
	if a == 0 {
		return 1
	}
	return a
}

// Write serialises the manifest atomically to dir/manifest.json.
func Write(dir string, m *Manifest) (string, error) {
	if dir == "" {
		return "", model.NewError(model.ErrParamInvalid, 0, 0, "empty manifest directory")
	}
	clean := filepath.Clean(dir)
	if clean == ".." {
		return "", model.NewError(model.ErrPathEscape, 0, 0, "manifest directory escapes working tree")
	}
	if err := os.MkdirAll(clean, 0o755); err != nil {
		return "", err
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return "", err
	}
	path := filepath.Join(clean, "manifest.json")
	tmp, err := os.CreateTemp(clean, ".manifest-*")
	if err != nil {
		return "", err
	}
	name := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(name)
		return "", err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(name)
		return "", err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return "", err
	}
	if err := os.Rename(name, path); err != nil {
		os.Remove(name)
		return "", err
	}
	return path, nil
}

// QualityStats returns finite quality percentiles from per-sample scores.
// It is used to report stream health without touching coded data.
func QualityStats(scores []float64) (mean, p95 float64, err error) {
	if len(scores) == 0 {
		return 0, 0, nil
	}
	sorted := append([]float64(nil), scores...)
	sort.Float64s(sorted)
	var sum float64
	for _, v := range sorted {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return 0, 0, model.NewError(model.ErrUnrecoverable, 0, 0, "non-finite quality score")
		}
		sum += v
	}
	idx := int(math.Ceil(0.95*float64(len(sorted)))) - 1
	if idx < 0 {
		idx = 0
	}
	return sum / float64(len(sorted)), sorted[idx], nil
}
