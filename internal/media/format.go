package media

import (
	"fmt"
	"strconv"
	"strings"
)

// FormatKV renders alternating key/value pairs as "k=v k=v", skipping empty
// keys. It is the single stable token formatter for every CLI report line.
func FormatKV(pairs ...string) string {
	var b strings.Builder
	for i := 0; i+1 < len(pairs); i += 2 {
		if pairs[i] == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(pairs[i])
		b.WriteByte('=')
		b.WriteString(pairs[i+1])
	}
	return b.String()
}

// FormatFloat renders v with the given number of decimals, without trailing
// exponent noise.
func FormatFloat(v float64, decimals int) string {
	return strconv.FormatFloat(v, 'f', decimals, 64)
}

// FormatInt renders an int64.
func FormatInt(v int64) string { return strconv.FormatInt(v, 10) }

// FormatIntN renders an int.
func FormatIntN(v int) string { return strconv.Itoa(v) }

// Percent renders a percentage error of actual against target.
func Percent(actual, target float64) float64 {
	if target == 0 {
		return 0
	}
	return (actual - target) / target * 100
}

// Errorf is a convenience wrapper that always returns a classified error.
func Errorf(code ErrorCode, format string, args ...any) *MediaError {
	return NewError(code, format, args...)
}

// MustParseRational is for fixtures and tests where a panic is acceptable.
func MustParseRational(s string) Rational {
	r, err := ParseRational(s)
	if err != nil {
		panic(fmt.Sprintf("media: %v", err))
	}
	return r
}
