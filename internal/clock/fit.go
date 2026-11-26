package clock

import (
	"fmt"
	"math"

	"mediad/internal/media"
)

// DriftLimitPPM is the threshold beyond which a fitted camera clock is
// reported as anomalous. An anomalous fit is never applied silently.
const DriftLimitPPM = 200.0

// Anchor pins a raw media timestamp to a host wall-clock reading.
type Anchor struct {
	HostUS     int64
	MediaTicks int64
}

// Fit is the least-squares clock model for one camera track. It maps a media
// time to host microseconds with HostUS = OffsetUS + Slope*MediaSeconds.
type Fit struct {
	OffsetUS float64
	Slope    float64 // host seconds per media second
	// DriftPPM is the camera clock's rate error against the host: positive
	// means the camera clock runs fast (it accumulates more media seconds per
	// host second), negative means it runs slow.
	DriftPPM float64
	Anchors  int
	Anomaly  bool
}

// FitModel estimates drift from sync anchors. One anchor yields a pure offset;
// two or more are least-squares fitted. The returned model is exact for the
// anchor set and deterministic given identical inputs.
func FitModel(anchors []Anchor, tb media.Rational) (*Fit, error) {
	if len(anchors) == 0 {
		return nil, media.NewError(media.ErrManifest, "no anchors to fit")
	}
	ms := func(t int64) float64 { return float64(t) * tb.Float64() }
	if len(anchors) == 1 {
		a := anchors[0]
		off := float64(a.HostUS) - ms(a.MediaTicks)*1e6
		return &Fit{OffsetUS: off, Slope: 1, DriftPPM: 0, Anchors: 1}, nil
	}
	// Ordinary least squares over (x=media seconds, y=host seconds), centred
	// on the means to avoid catastrophic cancellation: host microseconds are
	// ~1e15, so the uncentred normal equations lose all slope precision.
	n := float64(len(anchors))
	var meanX, meanY float64
	for _, a := range anchors {
		meanX += ms(a.MediaTicks)
		meanY += float64(a.HostUS) / 1e6
	}
	meanX /= n
	meanY /= n
	var sxx, sxy float64
	for _, a := range anchors {
		dx := ms(a.MediaTicks) - meanX
		dy := float64(a.HostUS)/1e6 - meanY
		sxx += dx * dx
		sxy += dx * dy
	}
	if sxx < 1e-12 {
		// Degenerate: all anchors share a media time. Fall back to an offset.
		return &Fit{OffsetUS: meanY * 1e6, Slope: 1, Anchors: len(anchors)}, nil
	}
	slope := sxy / sxx
	intercept := meanY - slope*meanX
	fit := &Fit{OffsetUS: intercept * 1e6, Slope: slope, Anchors: len(anchors)}
	// Slope maps media seconds to host seconds. A slope below one means the
	// same media span occupies less host time, so the camera clock ticks fast:
	// drift = (1/slope - 1).
	if slope > 0 {
		fit.DriftPPM = (1/slope - 1) * 1e6
	}
	fit.Anomaly = math.Abs(fit.DriftPPM) > DriftLimitPPM
	// Residual check: reject anchors that disagree wildly, but keep the fit
	// deterministic and report rather than silently rewriting data.
	var maxResidual float64
	for _, a := range anchors {
		pred := intercept + slope*ms(a.MediaTicks)
		obs := float64(a.HostUS) / 1e6
		if r := math.Abs(pred - obs); r > maxResidual {
			maxResidual = r
		}
	}
	if maxResidual > 1.0 {
		return nil, media.NewError(media.ErrDrift, "anchor residual %.3fs exceeds 1s", maxResidual)
	}
	return fit, nil
}

// AppliedSlope returns the slope the pipeline will actually use. When the fit
// is anomalous drift is reported but not corrected, so the slope is pinned to
// 1. Offset is still applied so events stay on the host timeline.
func (f *Fit) AppliedSlope() float64 {
	if f.Anomaly {
		return 1
	}
	return f.Slope
}

// ToHostSeconds maps a raw media timestamp to a host-timeline time in seconds.
func (f *Fit) ToHostSeconds(mediaTicks int64, tb media.Rational) float64 {
	return f.OffsetUS/1e6 + f.AppliedSlope()*float64(mediaTicks)*tb.Float64()
}

// ToTimeline maps a raw media timestamp to a MediaTick on the shared timeline.
// The calculation is done in integer microseconds then converted exactly, so
// repeated runs produce bit-identical timestamps.
func (f *Fit) ToTimeline(mediaTicks int64, tb media.Rational) media.MediaTick {
	hostSec := f.ToHostSeconds(mediaTicks, tb)
	us := int64(math.Round(hostSec * 1e6))
	return media.MediaTick(roundDiv(us*media.TickRate, 1_000_000))
}

// String renders the model for diagnostics.
func (f *Fit) String() string {
	return fmt.Sprintf("slope=%.9f drift_ppm=%.3f anchors=%d anomaly=%t", f.Slope, f.DriftPPM, f.Anchors, f.Anomaly)
}

func roundDiv(a, b int64) int64 {
	if b == 0 {
		return 0
	}
	if a >= 0 {
		return (a + b/2) / b
	}
	return -((-a + b/2) / b)
}

// UnwrapNear maps a wrapped raw tick value into the unwrapped domain defined
// by a set of reference ticks (typically unwrapped anchor times). The epoch
// multiple of 1<<width is chosen so the result is closest to the nearest
// reference; with no references the folded value is returned in epoch zero.
// The mapping is deterministic and used to place edit ranges that cross a
// wrap boundary.
func UnwrapNear(raw int64, width int, refs []int64) (int64, error) {
	if width < 1 || width > 62 {
		return 0, media.NewError(media.ErrInvalidTimebase, "wrap width %d outside [1,62]", width)
	}
	period := int64(1) << uint(width)
	folded := raw % period
	if folded < 0 {
		folded += period
	}
	if len(refs) == 0 {
		return folded, nil
	}
	best := folded
	bestDist := int64(-1)
	for _, ref := range refs {
		k := (ref - folded) / period
		for _, kk := range []int64{k, k + 1} {
			cand := folded + kk*period
			d := abs64(cand - ref)
			if bestDist < 0 || d < bestDist {
				best, bestDist = cand, d
			}
		}
	}
	return best, nil
}
