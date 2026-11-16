package timeline

import (
	"fmt"
	"math"
	"strconv"
)

func GCD(a, b int64) int64 {
	if a < 0 {
		a = -a
	}
	if b < 0 {
		b = -b
	}
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

func LCM(a, b int64) (int64, error) {
	if a <= 0 || b <= 0 {
		return 0, fmt.Errorf("LCM requires positive integers, got %d and %d", a, b)
	}
	g := GCD(a, b)
	if a/g > math.MaxInt64/b {
		return 0, fmt.Errorf("LCM overflow for %d and %d", a, b)
	}
	return (a / g) * b, nil
}

// ScaleTicks converts ticks between integer timebases without using floats.
func ScaleTicks(ticks, from, to int64) (int64, error) {
	if from <= 0 || to <= 0 {
		return 0, fmt.Errorf("timebases must be positive, got %d and %d", from, to)
	}
	g := GCD(from, to)
	from /= g
	to /= g
	if ticks == 0 {
		return 0, nil
	}
	if ticks > math.MaxInt64/to || ticks < math.MinInt64/to {
		return 0, fmt.Errorf("timebase scaling overflow")
	}
	return (ticks * to) / from, nil
}

func MillisToTicks(milliseconds, timescale int64) (int64, error) {
	if milliseconds < 0 || timescale <= 0 {
		return 0, fmt.Errorf("milliseconds must be non-negative and timescale positive")
	}
	g := GCD(timescale, 1000)
	numerator := timescale / g
	denominator := int64(1000) / g
	if milliseconds > math.MaxInt64/numerator {
		return 0, fmt.Errorf("millisecond conversion overflow")
	}
	return (milliseconds * numerator) / denominator, nil
}

func Abs(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

// FormatMillis3 converts ticks to a fixed three-decimal millisecond string.
// It is the only reporting conversion and never stores a floating-point value.
func FormatMillis3(ticks, timescale int64) string {
	if timescale <= 0 {
		return "0.000"
	}
	negative := ticks < 0
	if negative {
		ticks = -ticks
	}
	micros := ticks / timescale * 1_000_000
	if remainder := ticks % timescale; remainder != 0 {
		micros += remainder * 1_000_000 / timescale
	}
	whole, frac := micros/1000, micros%1000
	text := strconv.FormatInt(whole, 10) + "." + fmt.Sprintf("%03d", frac)
	if negative && micros != 0 {
		return "-" + text
	}
	return text
}

func FormatISO8601(ticks, timescale int64) string {
	if timescale <= 0 {
		return "PT0.000S"
	}
	if ticks < 0 {
		ticks = -ticks
	}
	micros := ticks / timescale * 1_000_000
	if remainder := ticks % timescale; remainder != 0 {
		micros += remainder * 1_000_000 / timescale
	}
	seconds, millis := micros/1_000_000, (micros/1000)%1000
	return "PT" + strconv.FormatInt(seconds, 10) + "." +
		fmt.Sprintf("%03d", millis) + "S"
}
