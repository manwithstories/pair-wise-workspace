// Package audio analyzes PCM sample streams for silence. Threshold and
// minimum duration are configurable, output ranges use the unified timeline,
// and a missing/empty audio track is reported as a quality error rather than
// being misclassified as silence.
package audio

import (
	"errors"
	"math"

	"mediacq/internal/clock"
)

// ErrNoAudio is the quality error raised when silence analysis is requested
// for a track that carries no PCM samples.
var ErrNoAudio = errors.New("audio: no PCM samples available; cannot assess silence")

// Config parameterizes silence detection.
type Config struct {
	// AmplitudeThreshold is the absolute sample level at or below which a
	// frame counts as silent, expressed in normalized [0,1] units.
	AmplitudeThreshold float64
	// MinDurationNS is the shortest silent span worth reporting.
	MinDurationNS int64
	// BytesPerFrame is the PCM frame width used for slicing the payload.
	BytesPerFrame int
}

// DefaultConfig returns broadcast-safe defaults.
func DefaultConfig() Config {
	return Config{AmplitudeThreshold: 0.01, MinDurationNS: 100_000_000, BytesPerFrame: 4}
}

// Frame is one PCM frame positioned on the unified timeline. Payload holds
// raw little-endian signed integer samples (mono in fixtures).
type Frame struct {
	Time    clock.Time
	DurNS   int64
	Payload []byte
}

// Span is a detected silent interval.
type Span struct {
	Range    clock.Range
	Samples  int
}

// Detect scans ordered PCM frames and merges consecutive silent frames into
// spans, dropping any shorter than MinDurationNS.
func Detect(frames []Frame, cfg Config) ([]Span, error) {
	if len(frames) == 0 {
		return nil, ErrNoAudio
	}
	if cfg.BytesPerFrame <= 0 {
		cfg = DefaultConfig()
	}
	spans := make([]Span, 0)
	var start clock.Time
	var silentStart bool
	var count int

	flush := func(end clock.Time) {
		r := clock.Range{Start: start, End: end}
		if end-start >= clock.Time(cfg.MinDurationNS) {
			spans = append(spans, Span{Range: r, Samples: count})
		}
	}

	for _, fr := range frames {
		silent := frameSilent(fr.Payload, cfg.BytesPerFrame, cfg.AmplitudeThreshold)
		switch {
		case silent && !silentStart:
			silentStart = true
			start = fr.Time
			count = 1
		case silent && silentStart:
			count++
		case !silent && silentStart:
			flush(fr.Time)
			silentStart = false
			count = 0
		}
	}
	if silentStart {
		last := frames[len(frames)-1]
		flush(last.Time + clock.Time(last.DurNS))
	}
	return spans, nil
}

// frameSilent returns true only when every integer sample in the frame is at
// or below the normalized threshold. Payload width supports 16/24/32-bit.
func frameSilent(payload []byte, width int, threshold float64) bool {
	if len(payload) == 0 {
		return true
	}
	if width <= 0 || len(payload)%width != 0 {
		width = 4
	}
	limit := normalizedLimit(width, threshold)
	for off := 0; off+width <= len(payload); off += width {
		v := decodeSigned(payload[off : off+width])
		if math.Abs(float64(v)) > limit {
			return false
		}
	}
	return true
}

func normalizedLimit(width int, threshold float64) float64 {
	max := float64(int64(1) << uint((width*8)-1))
	return max * threshold
}

// decodeSigned reads a little-endian signed integer of 2, 3 or 4 bytes. The
// fixture writer stores synthetic PCM byte streams; width 4 is the common path.
func decodeSigned(b []byte) int64 {
	switch len(b) {
	case 2:
		return int64(int16(uint16(b[0]) | uint16(b[1])<<8))
	case 3:
		v := uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16
		if v&0x800000 != 0 {
			v |= 0xff000000
		}
		return int64(int32(v))
	default:
		v := uint64(0)
		for i := len(b) - 1; i >= 0; i-- {
			v = (v << 8) | uint64(b[i])
		}
		// Sign-extend if the top bit is set.
		if v&(uint64(1)<<uint(len(b)*8-1)) != 0 {
			v |= ^uint64(0) << uint(len(b)*8)
		}
		return int64(v)
	}
}
