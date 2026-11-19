package clock

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Timecode is a parsed SMPTE label HH:MM:SS:FF (or ';' / '.' separated).
type Timecode struct{ H, M, S, F int64 }

var errBadTC = errors.New("malformed SMPTE timecode")

// ParseSMPTE parses a label without converting it. Separators ':', ';' and
// '.' are all accepted so both drop and non-drop notation round-trip.
func ParseSMPTE(label string) (Timecode, error) {
	parts := strings.FieldsFunc(strings.TrimSpace(label),
		func(c rune) bool { return c == ':' || c == ';' || c == '.' })
	if len(parts) != 4 {
		return Timecode{}, fmt.Errorf("%w: %q", errBadTC, label)
	}
	vals := [4]int64{}
	for i, p := range parts {
		n, err := strconv.ParseInt(p, 10, 64)
		if err != nil || n < 0 || len(p) > 2 {
			return Timecode{}, fmt.Errorf("%w: %q", errBadTC, label)
		}
		vals[i] = n
	}
	tc := Timecode{H: vals[0], M: vals[1], S: vals[2], F: vals[3]}
	if tc.H > 23 || tc.M > 59 || tc.S > 59 {
		return Timecode{}, fmt.Errorf("%w: field out of range in %q", errBadTC, label)
	}
	return tc, nil
}

// Validate rejects frame numbers that cannot exist on the given rate,
// including the dropped frame numbers of 29.97 drop-frame counting.
func (tc Timecode) Validate(r Rate) error {
	nom := r.NominalFPS()
	if tc.F >= nom {
		return fmt.Errorf("%w: frame %02d exceeds max %02d for rate %d/%d",
			errBadTC, tc.F, nom-1, r.Num, r.Den)
	}
	if r.Drop && tc.S == 0 && tc.M%10 != 0 && (tc.F == 0 || tc.F == 1) {
		return fmt.Errorf("%w: %02d:%02d:%02d;%02d uses a dropped frame number",
			errBadTC, tc.H, tc.M, tc.S, tc.F)
	}
	return nil
}

// String renders the label HH:MM:SS:FF, switching only the final separator
// to ';' for drop-frame counting per common SMPTE notation.
func (tc Timecode) String(r Rate) string {
	last := byte(':')
	if r.Drop {
		last = ';'
	}
	return fmt.Sprintf("%02d:%02d:%02d%c%02d", tc.H, tc.M, tc.S, last, tc.F)
}

// FramesPerDay is the count of frame positions between midnight boundaries
// under the rate's counting convention.
func FramesPerDay(r Rate) int64 {
	nom := r.NominalFPS()
	if r.Drop {
		// 107892 frames per real hour under 29.97 drop-frame.
		return 107892 * 24
	}
	return nom * 86400
}

// Frame counts frame positions since midnight for a validated label.
func (tc Timecode) Frame(r Rate) (int64, error) {
	if err := tc.Validate(r); err != nil {
		return 0, err
	}
	nom := r.NominalFPS()
	totalMinutes := tc.H*60 + tc.M
	base := (totalMinutes*60 + tc.S) * nom
	if r.Drop {
		// Two frame numbers skipped at each minute except every tenth.
		base -= (totalMinutes - totalMinutes/10) * 2
	}
	return base + tc.F, nil
}

// FrameToSMPTE turns a midnight-relative frame count back into a label.
func FrameToSMPTE(r Rate, frame int64) (Timecode, error) {
	if frame < 0 {
		return Timecode{}, fmt.Errorf("negative frame count %d", frame)
	}
	nom := r.NominalFPS()
	if r.Drop {
		// SMPTE 12M reconstruction via the 10-minute cycle. A cycle holds
		// 17982 counts but spans 18000 nominal frame positions: minute 0 has
		// all 1800 labels, minutes 1..9 each skip the labels ;00 and ;01.
		const (
			blockCounts  = 17982
			blockNominal = 18000
			minuteCounts = 1798
		)
		blocks := frame / blockCounts
		rem := frame % blockCounts
		if rem < 1800 {
			frame = blocks*blockNominal + rem
		} else {
			r2 := rem - 1800
			minute := 1 + r2/minuteCounts
			within := r2 % minuteCounts
			frame = blocks*blockNominal + minute*1800 + within + 2
		}
	}
	if frame >= FramesPerDay(r) {
		return Timecode{}, fmt.Errorf("frame %d crosses the day boundary", frame)
	}
	frames := frame % nom
	secs := frame / nom
	return Timecode{H: secs / 3600, M: (secs / 60) % 60, S: secs % 60, F: frames}, nil
}

// ParseSMPTETick converts a label directly to archive ticks. When wrap is
// true labels at or beyond the day boundary roll over instead of failing.
func ParseSMPTETick(r Rate, label string, wrap bool) (Tick, error) {
	tc, err := ParseSMPTE(label)
	if err != nil {
		return 0, err
	}
	frame, err := tc.Frame(r)
	if err != nil {
		return 0, err
	}
	if frame >= FramesPerDay(r) {
		if !wrap {
			return 0, fmt.Errorf("%w: %q crosses the day boundary", errOverflow, label)
		}
		frame %= FramesPerDay(r)
	}
	return FrameToTick(r, frame)
}

// TickToSMPTE renders an instant as a label, folding over midnight when wrap
// is enabled and rejecting cross-day instants otherwise.
func TickToSMPTE(r Rate, t Tick, wrap bool) (string, error) {
	if t >= Tick(TickPerDay) {
		if !wrap {
			return "", fmt.Errorf("%w: instant %d crosses the day boundary", errOverflow, t)
		}
		t = WrapDay(t)
	}
	frame, err := TickToFrame(r, t)
	if err != nil {
		return "", err
	}
	tc, err := FrameToSMPTE(r, frame)
	if err != nil {
		return "", err
	}
	return tc.String(r), nil
}
