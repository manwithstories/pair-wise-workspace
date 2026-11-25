// Package timeline aligns multiple camera tracks onto the unified 90 kHz axis.
// It estimates and corrects per-camera clock drift, keeps DTS/PTS and sample
// durations so variable frame rate is preserved, and confines anomalous clock
// jumps to the track that produced them.
package timeline

import (
	"math"
	"sort"

	"mediad/internal/media"
)

// TrackSamples is one camera track's samples in presentation order.
type TrackSamples struct {
	Key       media.TrackKey `json:"key"`
	Timescale int64          `json:"timescale"`
	Samples   []media.Sample `json:"-"`
}

// CameraFit is the drift correction applied to one camera.
type CameraFit struct {
	Camera        string  `json:"camera"`
	SlopePPM      float64 `json:"slope_ppm"`
	AnchorRef     int64   `json:"anchor_ref"`
	AnchorLocal   int64   `json:"anchor_local"`
	BoundaryErrMS float64 `json:"boundary_err_ms"`
	MaxErrMS      float64 `json:"max_err_ms"`
	Jumps         int     `json:"jumps"`
	Inliers       int     `json:"inliers"`
	Outliers      int     `json:"outliers"`
}

// Result is the aligned sample set plus the fit summary.
type Result struct {
	Tracks    []*TrackSamples `json:"-"`
	Fits      []CameraFit     `json:"fits"`
	P95SyncMS float64         `json:"p95_sync_ms"`
	MaxSyncMS float64         `json:"max_sync_ms"`
}

// AlignOptions tunes alignment.
type AlignOptions struct {
	// JumpThresholdMS is the residual above which a sample is treated as a
	// transient clock discontinuity rather than drift. It is detected and
	// erased per camera, so it cannot shift other tracks.
	JumpThresholdMS float64
}

func (o *AlignOptions) defaults() {
	if o.JumpThresholdMS <= 0 {
		o.JumpThresholdMS = 500
	}
}

// Align corrects per-camera drift and rebases samples onto the reference
// camera's unified axis. The reference is the camera whose key sorts first,
// giving deterministic output. Cameras are paired by acquisition index: a
// multi-camera capture shares a common frame grid, so index i on every camera
// denotes the same capture instant even when frame durations vary (VFR).
func Align(tracks []*TrackSamples, opts AlignOptions) (*Result, error) {
	opts.defaults()
	if len(tracks) == 0 {
		return nil, media.New(media.CodeNoTrack, "align", media.SourcePos{}, "no tracks to align")
	}
	sorted := append([]*TrackSamples(nil), tracks...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Key.Less(sorted[j].Key) })
	ref := sorted[0]
	if len(ref.Samples) == 0 {
		return nil, media.New(media.CodeNoTrack, "align", media.SourcePos{}, "reference track has no samples")
	}

	res := &Result{Tracks: make([]*TrackSamples, 0, len(sorted))}
	var errsMS []float64
	for ti, tr := range sorted {
		out := &TrackSamples{Key: tr.Key, Timescale: tr.Timescale, Samples: append([]media.Sample(nil), tr.Samples...)}
		if ti == 0 {
			fit := CameraFit{Camera: tr.Key.Camera, Inliers: len(tr.Samples)}
			if len(tr.Samples) > 0 {
				fit.AnchorLocal = tr.Samples[0].DTS
				fit.AnchorRef = tr.Samples[0].DTS
			}
			res.Fits = append(res.Fits, fit)
			res.Tracks = append(res.Tracks, out)
			continue
		}
		fit, jumps := fitCamera(tr, ref, opts)
		fit.Camera = tr.Key.Camera
		res.Fits = append(res.Fits, fit)
		errsMS = append(errsMS, applyCorrection(out, ref, fit, jumps)...)
		res.Tracks = append(res.Tracks, out)
	}
	if len(errsMS) > 0 {
		res.P95SyncMS = percentile(errsMS, 95)
		res.MaxSyncMS = maxOf(errsMS)
	}
	return res, nil
}

// fitCamera estimates the linear relationship ref ≈ slope*local + intercept by
// pairing samples on acquisition index. The robust estimator discards transient
// jumps, and any extrapolated residual above the jump threshold is recorded.
func fitCamera(tr, ref *TrackSamples, opts AlignOptions) (CameraFit, map[int]bool) {
	n := len(tr.Samples)
	var est media.DriftEstimator
	limit := n
	if len(ref.Samples) < limit {
		limit = len(ref.Samples)
	}
	for i := 0; i < limit; i++ {
		est.Add(ref.Samples[i].DTS, tr.Samples[i].DTS)
	}
	fit := CameraFit{}
	if est.Len() >= 2 {
		if d, err := est.Fit(); err == nil {
			fit.SlopePPM = d.SlopePPM
			fit.AnchorRef = d.AnchorRef
			fit.AnchorLocal = d.AnchorLocal
			fit.BoundaryErrMS = d.BoundaryErrMS
			fit.MaxErrMS = d.MaxErrMS
			fit.Inliers = d.Inliers
			fit.Outliers = d.Outliers
		}
	}

	jumps := make(map[int]bool)
	if fit.Inliers < 2 {
		return fit, jumps
	}
	slope := 1 + fit.SlopePPM/1e6
	for i := 0; i < n; i++ {
		local := float64(tr.Samples[i].DTS)
		modelRef := float64(fit.AnchorRef) + slope*(local-float64(fit.AnchorLocal))
		if i < len(ref.Samples) {
			residMS := math.Abs(float64(ref.Samples[i].DTS)-modelRef) / (float64(media.TickRate) / 1000)
			if residMS > opts.JumpThresholdMS {
				jumps[i] = true
				fit.Jumps++
			}
		}
	}
	return fit, jumps
}

// applyCorrection rewrites DTS/PTS to the reference axis while preserving each
// sample's own duration and PTS-DTS offset. Jump samples are snapped to the
// fitted line, erasing the discontinuity without letting it bias the model.
// Returns per-sample sync error in milliseconds.
func applyCorrection(out, ref *TrackSamples, fit CameraFit, jumps map[int]bool) []float64 {
	slope := 1 + fit.SlopePPM/1e6
	if fit.Inliers < 2 {
		return nil // no usable model; leave the track untouched
	}
	var errs []float64
	for i := range out.Samples {
		s := &out.Samples[i]
		cto := s.PTS - s.DTS
		newDTS := int64(math.Round(float64(fit.AnchorRef) + slope*float64(s.DTS-fit.AnchorLocal)))
		s.DTS = newDTS
		s.PTS = newDTS + cto
		if i < len(ref.Samples) && !jumps[i] {
			e := math.Abs(float64(ref.Samples[i].DTS-newDTS)) / (float64(media.TickRate) / 1000)
			errs = append(errs, e)
		}
	}
	return errs
}

func percentile(v []float64, p float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	idx := int(math.Ceil(p/100*float64(len(s)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(s) {
		idx = len(s) - 1
	}
	return s[idx]
}

func maxOf(v []float64) float64 {
	m := 0.0
	for _, x := range v {
		if x > m {
			m = x
		}
	}
	return m
}
