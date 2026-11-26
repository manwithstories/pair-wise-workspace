package media

// Media clock constants. All timestamps live on the same 90 kHz clock,
// including 48 kHz audio which is mapped by integer sample counting.
const (
	ClockRate = 90000           // video clock ticks per second
	AudioRate = 48000           // PCM samples per second
	VideoFPS  = 30              // frames per second used by the generated asset
	PTSModulo = uint64(1) << 33 // MPEG-TS style 33-bit PTS wraparound

	// AudioPacketsPerSecond makes eight audio packets align with one
	// one-second segment and one FEC block, keeping the timeline gap-free
	// at block boundaries.
	AudioPacketsPerSecond = 8
	AudioSamplesPerPacket = AudioRate / AudioPacketsPerSecond // 6000
)

// VideoPTS maps a frame index at VideoFPS onto the 90 kHz clock.
func VideoPTS(frame uint32) uint64 {
	return uint64(frame) * ClockRate / VideoFPS
}

// AudioPTS maps the first sample index of an audio packet onto the clock.
func AudioPTS(firstSample int) uint64 {
	return uint64(firstSample) * ClockRate / AudioRate
}

// FromSeconds converts a duration in seconds to clock ticks.
func FromSeconds(seconds float64) uint64 {
	return uint64(seconds*ClockRate + 0.5)
}

// ToSeconds converts clock ticks to seconds.
func ToSeconds(pts uint64) float64 {
	return float64(pts) / ClockRate
}

// Unwrap expands a raw 33-bit PTS into a monotonic 64-bit timestamp given
// the previous unwrapped value. The boolean reports whether the raw value
// crossed the modulus boundary backwards relative to prev (a wraparound).
func Unwrap(raw, prev uint64) (uint64, bool) {
	raw %= PTSModulo
	base := prev &^ (PTSModulo - 1)
	// Candidate on the same cycle, plus one cycle forward and back.
	candidates := []uint64{base + raw, base + raw + PTSModulo}
	if base >= PTSModulo {
		candidates = append(candidates, base+raw-PTSModulo)
	}
	best := candidates[0]
	for _, c := range candidates[1:] {
		if absDiff(c, prev) < absDiff(best, prev) {
			best = c
		}
	}
	// A forward wraparound: the nearest candidate lies one cycle above
	// prev while the raw value dropped below the previous raw value.
	wrapped := best > prev && raw < prev%PTSModulo
	return best, wrapped
}

func absDiff(a, b uint64) uint64 {
	if a > b {
		return a - b
	}
	return b - a
}

// ClockDiff returns the signed, wraparound-aware difference a-b in clock
// ticks. Positive means a is later than b.
func ClockDiff(a, b uint64) int64 {
	d := (a - b) & (PTSModulo - 1)
	if d > PTSModulo/2 {
		return int64(d) - int64(PTSModulo)
	}
	return int64(d)
}

// ClockLess reports whether timestamp a precedes b, wraparound aware.
func ClockLess(a, b uint64) bool { return ClockDiff(a, b) < 0 }

// ClockEqual reports whether two raw timestamps denote the same instant.
func ClockEqual(a, b uint64) bool { return a%PTSModulo == b%PTSModulo }
