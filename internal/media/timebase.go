package media

import (
	"encoding/binary"
	"math"
	"time"
)

// ClockRate is the shared normalized media clock used by audio, video and QC.
const ClockRate = 90_000

// DurationToTicks converts a wall duration to the 90 kHz media clock.
func DurationToTicks(d time.Duration) uint64 {
	return uint64(math.Round(d.Seconds() * float64(ClockRate)))
}

// TicksToDuration converts 90 kHz ticks to time.Duration.
func TicksToDuration(ticks uint64) time.Duration {
	return time.Duration(float64(ticks) / float64(ClockRate) * float64(time.Second))
}

// TicksToMS converts 90 kHz ticks to fractional milliseconds.
func TicksToMS(ticks uint64) float64 {
	return float64(ticks) * 1000 / ClockRate
}

// TimestampExtender turns wrapping 32-bit RTP timestamps into monotonic values.
type TimestampExtender struct {
	base     uint32
	extended uint64
	started  bool
}

// Extend maps ts to the nearest monotonic timestamp after the previous value.
func (e *TimestampExtender) Extend(ts uint32) uint64 {
	if !e.started {
		e.base = ts
		e.extended = uint64(ts)
		e.started = true
		return e.extended
	}

	candidate := (e.extended &^ 0xffff_ffff) + uint64(ts)
	best := candidate
	bestDist := absDiff(candidate, e.extended)
	if candidate >= 1<<32 {
		if lower := candidate - 1<<32; absDiff(lower, e.extended) < bestDist {
			best, bestDist = lower, absDiff(lower, e.extended)
		}
	}
	if upper := candidate + 1<<32; absDiff(upper, e.extended) < bestDist {
		best = upper
	}
	e.extended = best
	return best
}

func absDiff(a, b uint64) uint64 {
	if a > b {
		return a - b
	}
	return b - a
}

// AudioDescriptor describes frame timing parsed from codec-specific headers.
type AudioDescriptor struct {
	Codec         string
	Config        byte
	Stereo        bool
	Code          byte
	FrameCount    int
	FrameDuration time.Duration
	Duration      time.Duration
	ClockTicks    uint64
}

// ParseOpusTOC identifies the TOC configuration and total packet duration.
// See RFC 6716 sections 3.1 and 3.2.
func ParseOpusTOC(payload []byte) (AudioDescriptor, error) {
	const op = "media.ParseOpusTOC"
	if len(payload) == 0 {
		return AudioDescriptor{}, New(KindMalformed, op, "empty payload")
	}
	toc := payload[0]
	config := toc >> 3
	code := toc & 0x03
	frameMS := opusFrameMS(config)
	frames := 0

	switch code {
	case 0:
		frames = 1
	case 1:
		frames = 2
	case 2:
		frames = 2
	case 3:
		if len(payload) < 2 {
			return AudioDescriptor{}, New(KindMalformed, op, "truncated frame-count byte")
		}
		// Bits 0..5 of the second byte are the frame count.
		frames = int(payload[1] & 0x3f)
		if frames < 1 || frames > 48 {
			return AudioDescriptor{}, Wrap(KindMalformed, op, "invalid code-3 frame count", nil)
		}
	}

	frameDuration := time.Duration(frameMS * float64(time.Millisecond))
	total := time.Duration(float64(frames) * frameMS * float64(time.Millisecond))
	return AudioDescriptor{
		Codec: "opus", Config: config, Stereo: toc&0x04 != 0, Code: code,
		FrameCount: frames, FrameDuration: frameDuration, Duration: total,
		ClockTicks: DurationToTicks(total),
	}, nil
}

func opusFrameMS(config byte) float64 {
	switch {
	case config < 12: // SILK-only configurations 0..11
		return []float64{10, 20, 40, 60}[config%4]
	case config < 16: // Hybrid configurations 12..15
		return []float64{10, 20}[config%2]
	default: // CELT-only configurations 16..31
		return []float64{2.5, 5, 10, 20}[config%4]
	}
}

// AACFrameDuration returns the duration of AAC-LC frames with 1024 samples/AU.
func AACFrameDuration(sampleRateHz uint32, auCount int) (AudioDescriptor, error) {
	const op = "media.AACFrameDuration"
	if sampleRateHz == 0 {
		return AudioDescriptor{}, New(KindMalformed, op, "zero sample rate")
	}
	if auCount <= 0 || auCount > 64 {
		return AudioDescriptor{}, New(KindMalformed, op, "invalid AU count")
	}
	samples := uint64(1024 * auCount)
	d := time.Duration(float64(samples) / float64(sampleRateHz) * float64(time.Second))
	return AudioDescriptor{
		Codec: "aac", FrameCount: auCount, Duration: d,
		FrameDuration: time.Duration(float64(1024) / float64(sampleRateHz) * float64(time.Second)),
		ClockTicks:    uint64(math.Round(float64(samples) * ClockRate / float64(sampleRateHz))),
	}, nil
}

// ParseAACAUHeaders parses one or more 16-bit AAC-hbr AU headers and returns
// their AU sizes. RTP payload layout is defined by RFC 3640.
func ParseAACAUHeaders(payload []byte, sampleRateHz uint32) ([]int, AudioDescriptor, error) {
	const op = "media.ParseAACAUHeaders"
	if len(payload) < 4 {
		return nil, AudioDescriptor{}, New(KindMalformed, op, "truncated AU header section")
	}
	headerBits := int(binary.BigEndian.Uint16(payload[0:2]))
	if headerBits == 0 || headerBits%16 != 0 {
		return nil, AudioDescriptor{}, New(KindMalformed, op, "unsupported AU header length")
	}
	headerBytes := headerBits / 8
	if len(payload) < 2+headerBytes {
		return nil, AudioDescriptor{}, New(KindMalformed, op, "truncated AU headers")
	}
	count := headerBits / 16
	sizes := make([]int, 0, count)
	for i := 0; i < count; i++ {
		off := 2 + i*2
		bits := binary.BigEndian.Uint16(payload[off : off+2])
		size := int(bits >> 3) // 13-bit AU size; low 3 bits are AU index.
		if size == 0 {
			return nil, AudioDescriptor{}, New(KindMalformed, op, "zero AU size")
		}
		sizes = append(sizes, size)
	}
	desc, err := AACFrameDuration(sampleRateHz, count)
	if err != nil {
		return nil, AudioDescriptor{}, err
	}
	return sizes, desc, nil
}
