// Package quality computes frame PSNR, segment aggregates and regression
// decisions relative to a baseline report.
package quality

import (
	"fmt"
	"math"

	"gonum.org/v1/gonum/stat"

	"turncode/internal/yuv"
)

const UnlimitedPSNR = 100.0

// Segment is checkpoint-friendly streaming aggregation for one time segment.
type Segment struct {
	Index      int     `json:"index"`
	StartFrame int     `json:"start_frame"`
	Frames     int     `json:"frames"`
	SumPSNR    float64 `json:"sum_psnr"`
	SumSqPSNR  float64 `json:"sum_sq_psnr"`
	MinPSNR    float64 `json:"min_psnr"`
}

func (s Segment) Mean() float64 {
	if s.Frames == 0 {
		return 0
	}
	return s.SumPSNR / float64(s.Frames)
}

// Accumulator is O(segments), not O(frames), and is safe to serialize.
type Accumulator struct {
	SegmentFrames int       `json:"segment_frames"`
	TotalFrames   int       `json:"total_frames"`
	Segments      []Segment `json:"segments"`
}

func NewAccumulator(segmentFrames int) *Accumulator {
	return &Accumulator{SegmentFrames: segmentFrames}
}

// Add records one frame PSNR into its segment.
func (a *Accumulator) Add(frameIndex int, psnr float64) {
	if frameIndex < 0 || !isFinite(psnr) {
		return
	}
	idx := frameIndex / a.SegmentFrames
	for len(a.Segments) <= idx {
		i := len(a.Segments)
		a.Segments = append(a.Segments, Segment{
			Index:      i,
			StartFrame: i * a.SegmentFrames,
			MinPSNR:    UnlimitedPSNR,
		})
	}
	s := &a.Segments[idx]
	s.Frames++
	s.SumPSNR += psnr
	s.SumSqPSNR += psnr * psnr
	if psnr < s.MinPSNR {
		s.MinPSNR = psnr
	}
	a.TotalFrames++
}

// Merge joins a later checkpoint state without keeping per-frame values.
func (a *Accumulator) Clone() Accumulator {
	out := Accumulator{SegmentFrames: a.SegmentFrames, TotalFrames: a.TotalFrames}
	out.Segments = append([]Segment(nil), a.Segments...)
	return out
}

func (a *Accumulator) Mean() float64 {
	if a.TotalFrames == 0 {
		return 0
	}
	sum := 0.0
	for _, s := range a.Segments {
		sum += s.SumPSNR
	}
	return sum / float64(a.TotalFrames)
}

func (a *Accumulator) SegmentMeans() []float64 {
	out := make([]float64, 0, len(a.Segments))
	for _, s := range a.Segments {
		if s.Frames > 0 {
			out = append(out, s.Mean())
		}
	}
	return out
}

// SegmentStdDB is the sample standard deviation of segment means.
func (a *Accumulator) SegmentStdDB() float64 {
	means := a.SegmentMeans()
	if len(means) < 2 {
		return 0
	}
	_, std := stat.MeanStdDev(means, nil)
	return std
}

func (a *Accumulator) MinSegmentDB() float64 {
	min := math.Inf(1)
	for _, s := range a.Segments {
		if s.Frames > 0 && s.Mean() < min {
			min = s.Mean()
		}
	}
	if math.IsInf(min, 1) {
		return 0
	}
	return min
}

func (a *Accumulator) MinFrameDB() float64 {
	min := math.Inf(1)
	for _, s := range a.Segments {
		if s.Frames > 0 && s.MinPSNR < min {
			min = s.MinPSNR
		}
	}
	if math.IsInf(min, 1) {
		return 0
	}
	return min
}

// FramePSNR compares all three planes and returns dB. Identical frames are
// reported as UnlimitedPSNR to keep reports finite.
func FramePSNR(src, rec *yuv.Frame) (float64, error) {
	if err := src.Validate(); err != nil {
		return 0, err
	}
	if err := rec.Validate(); err != nil {
		return 0, err
	}
	if src.Width != rec.Width || src.Height != rec.Height {
		return 0, fmt.Errorf("quality: frame dimensions differ")
	}
	var sumSq float64
	for i := range src.Y {
		d := float64(src.Y[i]) - float64(rec.Y[i])
		sumSq += d * d
	}
	for i := range src.U {
		d := float64(src.U[i]) - float64(rec.U[i])
		sumSq += d * d
	}
	for i := range src.V {
		d := float64(src.V[i]) - float64(rec.V[i])
		sumSq += d * d
	}
	n := float64(len(src.Y) + len(src.U) + len(src.V))
	if sumSq == 0 {
		return UnlimitedPSNR, nil
	}
	mse := sumSq / n
	return 10 * math.Log10(255*255/mse), nil
}

// Summary is the subset of a baseline report needed for regression decisions.
type Summary struct {
	MeanDB        float64
	SegmentStdDB  float64
	MinDB         float64
	SegmentMeans  []float64
	SegmentFrames int
}

func (a *Accumulator) Summary() Summary {
	return Summary{
		MeanDB:        a.Mean(),
		SegmentStdDB:  a.SegmentStdDB(),
		MinDB:         a.MinSegmentDB(),
		SegmentMeans:  a.SegmentMeans(),
		SegmentFrames: a.SegmentFrames,
	}
}

type Regression struct {
	Regression       bool    `json:"regression"`
	DegradedSegments []int   `json:"degraded_segments,omitempty"`
	MeanDeltaDB      float64 `json:"mean_delta_db"`
	SegmentStdDelta  float64 `json:"segment_std_delta_db"`
}

// Compare flags a regression when mean PSNR falls by more than 0.5 dB or the
// spread of segment means rises by more than 0.3 dB.
func Compare(current, baseline Summary) Regression {
	out := Regression{
		MeanDeltaDB:     current.MeanDB - baseline.MeanDB,
		SegmentStdDelta: current.SegmentStdDB - baseline.SegmentStdDB,
	}
	out.Regression = out.MeanDeltaDB < -0.5 || out.SegmentStdDelta > 0.3
	if !out.Regression {
		return out
	}
	n := len(current.SegmentMeans)
	if len(baseline.SegmentMeans) < n {
		n = len(baseline.SegmentMeans)
	}
	for i := 0; i < n; i++ {
		drop := baseline.SegmentMeans[i] - current.SegmentMeans[i]
		if drop > 0.5 || (drop > 0.2 && out.MeanDeltaDB < -0.5) {
			out.DegradedSegments = append(out.DegradedSegments, i)
		}
	}
	if len(out.DegradedSegments) == 0 {
		for i := 0; i < n; i++ {
			if current.SegmentMeans[i] < baseline.MeanDB-0.5 {
				out.DegradedSegments = append(out.DegradedSegments, i)
			}
		}
	}
	return out
}

func isFinite(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0)
}
