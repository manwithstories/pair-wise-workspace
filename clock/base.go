// Package clock defines rational frame rates and SMPTE/LTC/VITC timecode math.
package clock

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
)

// Rate is an exact rational frame rate in frames per second.
type Rate struct {
	Num int64 `json:"num"`
	Den int64 `json:"den"`
}

// Supported broadcast frame rates.
var (
	Rate24         = Rate{Num: 24, Den: 1}
	Rate25         = Rate{Num: 25, Den: 1}
	Rate30         = Rate{Num: 30, Den: 1}
	Rate30000_1001 = Rate{Num: 30000, Den: 1001}
)

var timecodePattern = regexp.MustCompile(`^(\d{2}):(\d{2}):(\d{2})([:;. ])(\d{2})$`)

// Validate reports whether the rational rate is usable.
func (r Rate) Validate() error {
	if r.Num <= 0 || r.Den <= 0 {
		return fmt.Errorf("clock: invalid frame rate %d/%d", r.Num, r.Den)
	}
	return nil
}

// FPS returns the floating-point frame rate for display only.
func (r Rate) FPS() float64 { return float64(r.Num) / float64(r.Den) }

// TimecodeFPS returns the integer frame numbering base used by SMPTE timecode.
func (r Rate) TimecodeFPS() int {
	if r == Rate30000_1001 {
		return 30
	}
	return int(r.Num / r.Den)
}

// FramesToMillis converts a frame count to rounded nearest milliseconds.
func (r Rate) FramesToMillis(frames int64) int64 {
	if err := r.Validate(); err != nil {
		return 0
	}
	// frames / (Num/Den) seconds * 1000 = frames*Den*1000/Num.
	numerator := frames * r.Den * 1000
	if frames >= 0 {
		return (numerator + r.Num/2) / r.Num
	}
	return -((-numerator + r.Num/2) / r.Num)
}

// MillisToFrames converts milliseconds to a floor frame count.
func (r Rate) MillisToFrames(ms int64) int64 {
	if err := r.Validate(); err != nil {
		return 0
	}
	if ms < 0 {
		return -((-ms*r.Num + 1000*r.Den - 1) / (1000 * r.Den))
	}
	return ms * r.Num / (1000 * r.Den)
}

// Timecode is a validated SMPTE timecode. DropFrame is meaningful for 29.97.
type Timecode struct {
	H         int    `json:"h"`
	M         int    `json:"m"`
	S         int    `json:"s"`
	F         int    `json:"f"`
	Rate      Rate   `json:"rate"`
	DropFrame bool   `json:"drop_frame"`
	Source    string `json:"source,omitempty"` // LTC, VITC, or subtitle clock
}

// ParseTimecode parses HH:MM:SS:FF or HH:MM:SS;FF.
//
// An explicit dropFrame argument controls the expected mode. A ';' frame
// separator also marks a drop-frame timecode.
func ParseTimecode(value string, rate Rate, dropFrame bool) (Timecode, error) {
	if err := rate.Validate(); err != nil {
		return Timecode{}, err
	}
	m := timecodePattern.FindStringSubmatch(value)
	if m == nil {
		return Timecode{}, fmt.Errorf("clock: malformed timecode %q", value)
	}
	if m[4] == ";" || m[4] == "." {
		dropFrame = true
	}
	parts := make([]int, 0, 4)
	for _, raw := range []string{m[1], m[2], m[3], m[5]} {
		v, err := strconv.Atoi(raw)
		if err != nil {
			return Timecode{}, fmt.Errorf("clock: malformed timecode %q: %w", value, err)
		}
		parts = append(parts, v)
	}
	tc := Timecode{H: parts[0], M: parts[1], S: parts[2], F: parts[3], Rate: rate, DropFrame: dropFrame}
	if err := tc.Validate(); err != nil {
		return Timecode{}, err
	}
	return tc, nil
}

// Validate checks ranges and drop-frame skipped frame numbers.
func (tc Timecode) Validate() error {
	if err := tc.Rate.Validate(); err != nil {
		return err
	}
	if tc.H < 0 || tc.H > 23 || tc.M < 0 || tc.M > 59 || tc.S < 0 || tc.S > 59 {
		return fmt.Errorf("clock: timecode field out of range: %02d:%02d:%02d", tc.H, tc.M, tc.S)
	}
	maxFrame := tc.Rate.TimecodeFPS() - 1
	if tc.F < 0 || tc.F > maxFrame {
		return fmt.Errorf("clock: frame %d out of range 0..%d", tc.F, maxFrame)
	}
	if tc.DropFrame && tc.Rate != Rate30000_1001 {
		return fmt.Errorf("clock: drop-frame is only valid for 30000/1001, got %d/%d", tc.Rate.Num, tc.Rate.Den)
	}
	if tc.DropFrame && tc.S == 0 && tc.F < 2 && tc.M%10 != 0 {
		return fmt.Errorf("clock: illegal DF skipped frame %s", tc.String())
	}
	return nil
}

// String formats with ':' for NDF and ';' before frames for DF.
func (tc Timecode) String() string {
	sep := ":"
	if tc.DropFrame {
		sep = ";"
	}
	return fmt.Sprintf("%02d:%02d:%02d%s%02d", tc.H, tc.M, tc.S, sep, tc.F)
}

// DayFrames returns the number of frame indices in one timecode day.
func (tc Timecode) DayFrames() int64 {
	fps := int64(tc.Rate.TimecodeFPS())
	nominal := int64(24*60*60) * fps
	if tc.DropFrame {
		// Minutes 0..1439: two dropped frames except minutes divisible by ten.
		totalMinutes := int64(24 * 60)
		return nominal - 2*(totalMinutes-totalMinutes/10)
	}
	return nominal
}

// FrameIndex returns the continuous frame number since midnight after DF correction.
func (tc Timecode) FrameIndex() int64 {
	fps := int64(tc.Rate.TimecodeFPS())
	nominal := ((int64(tc.H)*60+int64(tc.M))*60+int64(tc.S))*fps + int64(tc.F)
	if !tc.DropFrame {
		return nominal
	}
	totalMinutes := int64(tc.H*60 + tc.M)
	return nominal - 2*(totalMinutes-totalMinutes/10)
}

// Millis returns the rational-wall-clock position represented by this timecode.
func (tc Timecode) Millis() int64 { return tc.Rate.FramesToMillis(tc.FrameIndex()) }

// Transition describes two adjacent source timecodes.
type Transition struct {
	DeltaFrames   int64 `json:"delta_frames"`
	CrossMidnight bool  `json:"cross_midnight"`
}

// CheckTransition accepts forward time and a 23:59 -> 00:00 midnight wrap.
// Any other smaller next value is a timecode regression.
func CheckTransition(prev, next Timecode) (Transition, error) {
	if prev.Rate != next.Rate || prev.DropFrame != next.DropFrame {
		return Transition{}, errors.New("clock: cannot compare timecodes with different rates or DF modes")
	}
	p, n := prev.FrameIndex(), next.FrameIndex()
	if n >= p {
		return Transition{DeltaFrames: n - p}, nil
	}
	if prev.H == 23 && next.H == 0 {
		return Transition{DeltaFrames: next.DayFrames() - p + n, CrossMidnight: true}, nil
	}
	return Transition{}, fmt.Errorf("clock: timecode regression %s -> %s", prev.String(), next.String())
}

// Alignment is the result of comparing LTC and VITC stamps.
type Alignment struct {
	DeltaFrames int64 `json:"delta_frames"`
	Aligned     bool  `json:"aligned"`
}

// CheckAlignment compares two stamps and allows toleranceFrames of skew.
func CheckAlignment(ltc, vitc Timecode, toleranceFrames int64) (Alignment, error) {
	if ltc.Rate != vitc.Rate || ltc.DropFrame != vitc.DropFrame {
		return Alignment{}, errors.New("clock: LTC/VITC rate or DF mode mismatch")
	}
	delta := vitc.FrameIndex() - ltc.FrameIndex()
	aligned := delta >= -toleranceFrames && delta <= toleranceFrames
	return Alignment{DeltaFrames: delta, Aligned: aligned}, nil
}
