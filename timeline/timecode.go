package timeline

import (
	"strconv"
	"strings"

	"mediad/diag"
)

// Timecode is a SMPTE-style HH:MM:SS:FF label. The Drop field records whether
// the label was expressed in drop-frame notation (semicolon before frames, or
// an explicit drop-frame rate).
type Timecode struct {
	Hour, Min, Sec, Frame int
	Drop                  bool
}

// nominalRoundedFPS is the integer frame count used in timecode labels.
func nominalRoundedFPS(fr FrameRate) int64 {
	switch fr.Name {
	case FR24.Name:
		return 24
	case FR25.Name:
		return 25
	default: // 30, 29.97 and 29.97-drop all label at 30 fps
		return 30
	}
}

// FramesPer24h is the total frame count at the day boundary.
func FramesPer24h(fr FrameRate) int64 {
	switch fr.Name {
	case FR24.Name:
		return 24 * 3600 * 24
	case FR25.Name:
		return 25 * 3600 * 24
	case FR2997.Name, FR2997DF.Name:
		return 107892 * 24 // exact NTSC day count
	default:
		return 30 * 3600 * 24
	}
}

// ParseTimecode reads "HH:MM:SS:FF" or drop-frame "HH:MM:SS;FF". A semicolon
// anywhere in the separator position forces drop-frame notation.
func ParseTimecode(s string, fr FrameRate) (Timecode, error) {
	drop := strings.Contains(s, ";")
	parts := strings.FieldsFunc(s, func(r rune) bool { return r == ':' || r == ';' })
	if len(parts) != 4 {
		return Timecode{}, diag.New(diag.ETimecode, "", "timecode %q must be HH:MM:SS:FF", s)
	}
	vals := [4]int{}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || len(p) != 2 || n < 0 {
			return Timecode{}, diag.New(diag.ETimecode, "", "timecode %q has illegal component %q", s, p)
		}
		vals[i] = n
	}
	tc := Timecode{Hour: vals[0], Min: vals[1], Sec: vals[2], Frame: vals[3], Drop: drop}
	if err := tc.validate(fr); err != nil {
		return Timecode{}, err
	}
	return tc, nil
}

func (tc Timecode) validate(fr FrameRate) error {
	fps := int(nominalRoundedFPS(fr))
	if tc.Hour > 23 || tc.Min > 59 || tc.Sec > 59 || tc.Frame >= fps {
		return diag.New(diag.ETimecode, "", "illegal timecode %02d:%02d:%02d:%02d for rate %s",
			tc.Hour, tc.Min, tc.Sec, tc.Frame, fr.Name)
	}
	// In drop-frame notation the first two frame numbers of every non-ten
	// minute are skipped and must never appear in a label.
	if fr.Drop && tc.Sec == 0 && tc.Min%10 != 0 && tc.Frame < 2 {
		return diag.New(diag.ETimecode, "", "dropped frame number %02d:%02d:%02d;%02d is illegal",
			tc.Hour, tc.Min, tc.Sec, tc.Frame)
	}
	return nil
}

// FrameIndex converts the label to a zero-based real-frame index. For
// drop-frame rates the skipped label numbers are removed, so consecutive real
// frames map to consecutive indices.
func (tc Timecode) FrameIndex(fr FrameRate) (int64, error) {
	if err := tc.validate(fr); err != nil {
		return 0, err
	}
	h, m, s, f := int64(tc.Hour), int64(tc.Min), int64(tc.Sec), int64(tc.Frame)
	if fr.Drop {
		return h*107892 + m*1798 + s*30 + f + 2*(m/10), nil
	}
	fps := nominalRoundedFPS(fr)
	return (h*3600+m*60+s)*fps + f, nil
}

// FrameToTimecode renders a real-frame index as a label, applying the 24-hour
// wrap deterministically: the returned label is always inside one day while
// the wrap count is exposed separately by WrapCount.
func FrameToTimecode(frame int64, fr FrameRate) (Timecode, error) {
	if frame < 0 {
		return Timecode{}, diag.New(diag.ETimecode, "", "negative frame index %d", frame)
	}
	day := FramesPer24h(fr)
	idx := frame % day
	if fr.Drop {
		return dropFrameLabel(idx), nil
	}
	fps := nominalRoundedFPS(fr)
	tc := Timecode{Drop: false}
	tc.Hour = int(idx / (fps * 3600))
	idx %= fps * 3600
	tc.Min = int(idx / (fps * 60))
	idx %= fps * 60
	tc.Sec = int(idx / fps)
	tc.Frame = int(idx % fps)
	return tc, nil
}

// dropFrameLabel implements the SMPTE 29.97 drop-frame inverse. A day splits
// into 144 ten-minute blocks of 17982 real frames (18000 label frames each).
// Inside a block minute 0 keeps all 1800 frames while minutes 1..9 start with
// two skipped label numbers. This derivation round-trips exactly with
// Timecode.Frame for every real frame index in a day.
func dropFrameLabel(idx int64) Timecode {
	d := idx / 17982
	r := idx % 17982
	var minute, frameInMinute int64
	if r < 1800 {
		minute, frameInMinute = 0, r
	} else {
		rr := r - 1800
		minute = 1 + rr/1798
		frameInMinute = rr%1798 + 2 // labels ;00 and ;01 were skipped
	}
	totalMinute := 10*d + minute
	tc := Timecode{Drop: true}
	tc.Hour = int(totalMinute / 60)
	tc.Min = int(totalMinute % 60)
	tc.Sec = int(frameInMinute / 30)
	tc.Frame = int(frameInMinute % 30)
	return tc
}

// WrapCount reports how many 24-hour boundaries a frame index has crossed.
func WrapCount(frame int64, fr FrameRate) int64 {
	if frame < 0 {
		return 0
	}
	return frame / FramesPer24h(fr)
}

// ShiftFrame applies a signed frame offset with deterministic day-boundary
// behavior: positive offsets wrap forward, negative offsets wrap backward into
// the previous day rather than producing an error. The second result is the
// net wrap count (negative when crossing midnight backwards).
func ShiftFrame(frame, offset int64, fr FrameRate) (int64, int64) {
	day := FramesPer24h(fr)
	sum := frame + offset
	wraps := floorDiv(sum, day)
	return floorMod(sum, day), wraps
}

func floorDiv(a, b int64) int64 {
	q := a / b
	if (a%b != 0) && ((a < 0) != (b < 0)) {
		q--
	}
	return q
}

func floorMod(a, b int64) int64 { return a - floorDiv(a, b)*b }

// String renders the canonical label, using ';' for drop-frame notation.
func (tc Timecode) String() string {
	sep := byte(':')
	if tc.Drop {
		sep = ';'
	}
	return two(tc.Hour) + ":" + two(tc.Min) + ":" + two(tc.Sec) + string(sep) + two(tc.Frame)
}

func two(n int) string {
	if n < 10 {
		return "0" + strconv.Itoa(n)
	}
	return strconv.Itoa(n)
}
