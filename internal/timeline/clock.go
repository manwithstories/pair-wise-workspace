// Package timeline provides the unified rational time base shared by all
// tracks: PTS values are converted to seconds as pts * Num / Den.
package timeline

import "fmt"

// Rational is a fraction Num/Den.
type Rational struct {
	Num int64 `json:"num"`
	Den int64 `json:"den"`
}

// Clock converts PTS values to seconds using a rational time base.
type Clock struct {
	TB Rational
}

// New validates the time base and returns a Clock.
func New(tb Rational) (Clock, error) {
	if tb.Num <= 0 || tb.Den <= 0 {
		return Clock{}, fmt.Errorf("invalid timebase %d/%d", tb.Num, tb.Den)
	}
	return Clock{TB: tb}, nil
}

// Seconds converts a PTS value to seconds.
func (c Clock) Seconds(pts int64) float64 {
	return float64(pts) * float64(c.TB.Num) / float64(c.TB.Den)
}

// StepPTS returns the PTS increment of one frame at the given frame rate.
// The step must be an integer in the clock's time base.
func (c Clock) StepPTS(fps Rational) (int64, error) {
	if fps.Num <= 0 || fps.Den <= 0 {
		return 0, fmt.Errorf("invalid fps %d/%d", fps.Num, fps.Den)
	}
	num := c.TB.Den * fps.Den
	den := c.TB.Num * fps.Num
	if num%den != 0 {
		return 0, fmt.Errorf("non-integer frame step: timebase %d/%d at %d/%d fps",
			c.TB.Num, c.TB.Den, fps.Num, fps.Den)
	}
	return num / den, nil
}

// HalfOpen is a half-open time range [Start, End) in seconds.
type HalfOpen struct {
	Start float64 `json:"start"`
	End   float64 `json:"end"`
}

// FrameRef pairs a frame sequence number with its PTS.
type FrameRef struct {
	Seq int64
	PTS int64
}

// Discontinuity describes a timestamp regression or a missing-frame gap.
type Discontinuity struct {
	Regression bool
	Seq        int64 // sequence number where the problem was detected
	Range      HalfOpen
	Missing    int64 // number of missing frames (gap only)
}

// Check verifies monotonic PTS and continuous sequence numbers against the
// expected PTS step. refs must be ordered by sequence number.
func (c Clock) Check(refs []FrameRef, step int64) []Discontinuity {
	var out []Discontinuity
	for i := 1; i < len(refs); i++ {
		prev, cur := refs[i-1], refs[i]
		if cur.PTS <= prev.PTS {
			out = append(out, Discontinuity{
				Regression: true,
				Seq:        cur.Seq,
				Range:      HalfOpen{Start: c.Seconds(cur.PTS), End: c.Seconds(prev.PTS + step)},
			})
			continue
		}
		delta := cur.Seq - prev.Seq
		if gap := delta - 1; gap > 0 || cur.PTS != prev.PTS+step*delta {
			missing := gap
			if missing < 0 {
				missing = 0
			}
			out = append(out, Discontinuity{
				Seq:     cur.Seq,
				Range:   HalfOpen{Start: c.Seconds(prev.PTS + step), End: c.Seconds(cur.PTS)},
				Missing: missing,
			})
		}
	}
	return out
}
