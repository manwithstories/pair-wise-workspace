package media

import "math/big"

// MediaTime is an exact rational position on a shared timeline, measured in
// seconds. All subsystems round sample and frame boundaries against the same
// rational base so audio cuts and video frames cannot drift apart.
type MediaTime struct {
	Num int64
	Den int64
}

// Shared pipeline constants. Audio is normalized to 48 kHz / 6 channels and
// video is rendered at 320x180 / 24 fps.
const (
	AudioRate     int64 = 48000
	VideoFPS      int64 = 24
	FrameWidth          = 320
	FrameHeight         = 180
	AudioChannels       = 6
)

// NewTime builds a normalized, non-negative MediaTime. Negative positions and
// a zero denominator are input errors; media time is never negative in this
// pipeline.
func NewTime(num, den int64) (MediaTime, *Error) {
	if den == 0 {
		return MediaTime{}, Input("time.zero_den", "denominator must not be zero")
	}
	if den < 0 {
		num, den = -num, -den
	}
	if num < 0 {
		return MediaTime{}, Input("time.negative", "negative media time %d/%d", num, den)
	}
	t := MediaTime{Num: num, Den: den}
	if g := gcd(t.Num, t.Den); g > 1 {
		t.Num /= g
		t.Den /= g
	}
	return t, nil
}

// MustTime builds a constant MediaTime for literals and tests.
func MustTime(num, den int64) MediaTime {
	t, err := NewTime(num, den)
	if err != nil {
		panic(err.Error())
	}
	return t
}

func gcd(a, b int64) int64 {
	for b != 0 {
		a, b = b, a%b
	}
	if a < 0 {
		a = -a
	}
	if a == 0 {
		return 1
	}
	return a
}

// Since returns t - o. Values are compared and subtracted exactly before any
// rounding happens.
func (t MediaTime) Since(o MediaTime) (MediaTime, *Error) {
	if t.Cmp(o) < 0 {
		return MediaTime{}, Input("time.reversed", "time %s precedes %s", t, o)
	}
	l, lok := mulOK(t.Num, o.Den)
	r, rok := mulOK(o.Num, t.Den)
	if !lok || !rok {
		return MediaTime{}, Input("time.overflow", "subtracting %s from %s overflows", o, t)
	}
	d, dok := subOK(l, r)
	if !dok {
		return MediaTime{}, Input("time.overflow", "subtracting %s from %s overflows", o, t)
	}
	return NewTime(d, t.Den*o.Den)
}

// Add returns t + o.
func (t MediaTime) Add(o MediaTime) (MediaTime, *Error) {
	l, lok := mulOK(t.Num, o.Den)
	r, rok := mulOK(o.Num, t.Den)
	if !lok || !rok {
		return MediaTime{}, Input("time.overflow", "adding %s and %s overflows", t, o)
	}
	s, sok := addOK(l, r)
	if !sok {
		return MediaTime{}, Input("time.overflow", "adding %s and %s overflows", t, o)
	}
	return NewTime(s, t.Den*o.Den)
}

// Cmp compares t and o exactly: -1, 0, +1.
func (t MediaTime) Cmp(o MediaTime) int {
	l := new(big.Int).Mul(big.NewInt(t.Num), big.NewInt(o.Den))
	r := new(big.Int).Mul(big.NewInt(o.Num), big.NewInt(t.Den))
	return l.Cmp(r)
}

func (t MediaTime) Less(o MediaTime) bool  { return t.Cmp(o) < 0 }
func (t MediaTime) Equal(o MediaTime) bool { return t.Cmp(o) == 0 }

// Float is for display and latency metrics only; boundary math stays rational.
func (t MediaTime) Float() float64 { return float64(t.Num) / float64(t.Den) }

// Units returns how many whole units (samples or frames) fit in t, rounded
// half-up. Rounding to the nearest unit means a cut is never more than half a
// unit away from the rational position.
func (t MediaTime) Units(units int64) (int64, *Error) {
	if units <= 0 {
		return 0, Input("time.bad_units", "units must be positive, got %d", units)
	}
	n, ok := mulOK(t.Num, units)
	if !ok {
		return 0, Input("time.overflow", "scaling %s by %d overflows", t, units)
	}
	// q = n / Den with half-up rounding on the exact remainder 2*r >= Den.
	q := n / t.Den
	r := n % t.Den
	if r > 0 && 2*r >= t.Den {
		if q == maxInt64 {
			return 0, Input("time.overflow", "rounding %s overflows", t)
		}
		q++
	}
	return q, nil
}

// SampleIndex is the nearest 48 kHz sample boundary.
func (t MediaTime) SampleIndex() (int64, *Error) { return t.Units(AudioRate) }

// FrameIndex is the nearest 24 fps frame boundary.
func (t MediaTime) FrameIndex() (int64, *Error) { return t.Units(VideoFPS) }

// String renders the reduced fraction as num/den.
func (t MediaTime) String() string {
	return itoa(t.Num) + "/" + itoa(t.Den)
}

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	neg := v < 0
	if neg {
		v = -v
	}
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

const (
	minInt64 = -1 << 63
	maxInt64 = 1<<63 - 1
)

// mulOK reports whether a*b fits int64.
func mulOK(a, b int64) (int64, bool) {
	if a == 0 || b == 0 {
		return 0, true
	}
	if a == -1 && b == minInt64 || b == -1 && a == minInt64 {
		return 0, false
	}
	p := a * b
	if p/b != a {
		return 0, false
	}
	return p, true
}

// addOK reports whether a+b fits int64.
func addOK(a, b int64) (int64, bool) {
	s := a + b
	if b > 0 && s < a || b < 0 && s > a {
		return 0, false
	}
	return s, true
}

// subOK reports whether a-b fits int64.
func subOK(a, b int64) (int64, bool) {
	if b == minInt64 {
		return 0, false
	}
	return addOK(a, -b)
}
