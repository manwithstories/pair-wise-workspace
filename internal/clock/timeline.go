package clock

import (
	"fmt"

	"mediad/internal/fault"
)

// Stamp is the first media timestamp covered by one source fragment.
type Stamp struct {
	SegmentIndex int
	Period       int
	PTS          Rational
	DTS          Rational
	Duration     Rational
}

// Period records one discontinuity-delimited timeline period.
type Period struct {
	Index        int
	InitSegment  string
	Start        Rational
	End          Rational
	SegmentCount int
}

// Timeline assembles fragments on one track. A discontinuity creates a new
// period; timestamps are checked independently inside each period.
type Timeline struct {
	Track   string
	Periods []Period

	seen        map[int]bool
	lastPTS     map[int]Rational
	lastDTS     map[int]Rational
	expectedEnd map[int]Rational
}

func NewTimeline(track string) *Timeline {
	return &Timeline{
		Track:       track,
		seen:        map[int]bool{},
		lastPTS:     map[int]Rational{},
		lastDTS:     map[int]Rational{},
		expectedEnd: map[int]Rational{},
	}
}

// Observe validates and appends one fragment timestamp.
func (tl *Timeline) Observe(path string, s Stamp, initSegment string) error {
	if s.Period < 0 {
		return fault.New(fault.TimestampOverflow, path, int64(s.SegmentIndex), "negative period")
	}
	if Cmp(s.Duration, Zero) <= 0 {
		return fault.New(fault.Boundary, path, int64(s.SegmentIndex), "segment duration must be positive")
	}
	if Cmp(s.PTS, Zero) < 0 || Cmp(s.DTS, Zero) < 0 {
		return fault.New(fault.TimestampBackward, path, int64(s.SegmentIndex), "negative timestamp")
	}
	if initSegment == "" {
		return fault.New(fault.InitMissing, path, int64(s.SegmentIndex),
			"period %d has no initialization segment after discontinuity", s.Period)
	}

	end, err := Add(s.PTS, s.Duration)
	if err != nil {
		return err
	}

	if !tl.seen[s.Period] {
		tl.seen[s.Period] = true
		tl.lastPTS[s.Period] = s.PTS
		tl.lastDTS[s.Period] = s.DTS
		tl.expectedEnd[s.Period] = end
		tl.Periods = append(tl.Periods, Period{
			Index: s.Period, InitSegment: initSegment,
			Start: s.PTS, End: end, SegmentCount: 1,
		})
		return nil
	}

	prevPTS := tl.lastPTS[s.Period]
	prevDTS := tl.lastDTS[s.Period]
	if Cmp(s.PTS, prevPTS) < 0 {
		return fault.New(fault.TimestampBackward, path, int64(s.SegmentIndex),
			"%s PTS moved backward in period %d: %s < %s", tl.Track, s.Period, s.PTS, prevPTS)
	}
	if Cmp(s.DTS, prevDTS) < 0 {
		return fault.New(fault.TimestampBackward, path, int64(s.SegmentIndex),
			"%s DTS moved backward in period %d: %s < %s", tl.Track, s.Period, s.DTS, prevDTS)
	}
	if want := tl.expectedEnd[s.Period]; Cmp(s.PTS, want) != 0 {
		return fault.New(fault.SegmentMismatch, path, int64(s.SegmentIndex),
			"%s segment boundary mismatch in period %d: pts=%s want=%s", tl.Track, s.Period, s.PTS, want)
	}

	tl.lastPTS[s.Period] = s.PTS
	tl.lastDTS[s.Period] = s.DTS
	tl.expectedEnd[s.Period] = end
	for i := range tl.Periods {
		if tl.Periods[i].Index == s.Period {
			tl.Periods[i].End = end
			tl.Periods[i].SegmentCount++
			break
		}
	}
	return nil
}

// Duration returns the sum of all period durations on the period-local timeline.
func (tl *Timeline) Duration() (Rational, error) {
	total := Zero
	for _, p := range tl.Periods {
		d, err := Sub(p.End, p.Start)
		if err != nil {
			return Rational{}, err
		}
		total, err = Add(total, d)
		if err != nil {
			return Rational{}, err
		}
	}
	return total, nil
}

// TimeRange is the JSON-friendly range recorded in the ledger.
type TimeRange struct {
	Track   string  `json:"track"`
	Period  int     `json:"period"`
	Start   string  `json:"start"`
	End     string  `json:"end"`
	Seconds float64 `json:"seconds"`
}

// Ranges returns per-period ranges suitable for provenance records.
func (tl *Timeline) Ranges() []TimeRange {
	out := make([]TimeRange, 0, len(tl.Periods))
	for _, p := range tl.Periods {
		d, _ := Sub(p.End, p.Start)
		out = append(out, TimeRange{
			Track: tl.Track, Period: p.Index,
			Start: p.Start.String(), End: p.End.String(), Seconds: d.Float64(),
		})
	}
	return out
}

// CheckOverflowMul is a test/helper proving extreme time bases are rejected.
func CheckOverflowMul(a, b Rational) error {
	_, err := Mul(a, b)
	if err != nil {
		return err
	}
	if a.N > (1<<62) || b.N > (1<<62) {
		return fault.New(fault.TimestampOverflow, "", -1, fmt.Sprintf("product %s*%s too large", a, b))
	}
	return nil
}
