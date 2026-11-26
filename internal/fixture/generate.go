package fixture

import (
	"fmt"
	"time"
)

// ClipSpec describes one generated delivery package.
type ClipSpec struct {
	Name            string
	DurationSec     float64
	VideoFPSNum     int64
	VideoFPSDen     int64
	TargetBps       int64
	AudioSampleRate int
	// AudioSamples is the total audio sampling count, used to derive the
	// audio packet grid independently of the video grid.
	AudioSamples  int
	StartPTS      int64 // first presentation instant, track ticks (90 kHz)
	AudioStartPTS int64
	KeyframeEvery int // video frames per keyframe
	// CMEAF selects fMP4 output (one init section plus media segments).
	CMAF bool
}

// DefaultClipSpec is the demo clip: 10 s, 300 video frames, 480000 audio
// samples at a 2 Mbps nominal rate.
func DefaultClipSpec() ClipSpec {
	return ClipSpec{
		Name:            "clip",
		DurationSec:     10,
		VideoFPSNum:     30,
		VideoFPSDen:     1,
		TargetBps:       2000000,
		AudioSampleRate: 48000,
		AudioSamples:    480000,
		KeyframeEvery:   30,
	}
}

// FrameCount returns the number of video frames the clip declares.
func (s ClipSpec) FrameCount() int64 {
	return int64(s.DurationSec*float64(s.VideoFPSNum)/float64(s.VideoFPSDen)) + 0
}

// VideoTickDur is the presentation duration of one frame in 90 kHz ticks.
func (s ClipSpec) VideoTickDur() int64 {
	return int64(VideoTimescale) * s.VideoFPSDen / s.VideoFPSNum
}

// AudioTickDur is the duration of one audio frame in 90 kHz ticks, the
// timescale carried by PES timestamps.
func (s ClipSpec) AudioTickDur() int64 {
	return int64(AudioSamplesPerFrame) * VideoTimescale / AudioTimescale
}

// PayloadFor deterministically synthesises payload bytes for a sample. The
// bytes depend only on (kind, index, length), so two independent runs produce
// identical payloads and therefore identical payload hashes.
func PayloadFor(kind string, index, length int) []byte {
	out := make([]byte, length)
	var x uint32 = 0x9E3779B9 ^ uint32(index)<<8
	if kind == "audio" {
		x ^= 0xA5A5A5A5
	}
	for i := range out {
		x = x*1664525 + 1013904223
		out[i] = byte(x >> 24)
	}
	return out
}

// BuildTS constructs a transport-stream segment covering [frameStart,
// frameStart+frames) video frames and [audioStart, audioStart+audioFrames)
// audio frames. PTS/DTS are absolute 90 kHz values.
func (s ClipSpec) BuildTS(frameStart, frames, audioStart, audioFrames int) []byte {
	w := NewTSWriter()
	w.WritePAT()
	w.WritePMT()
	vDur := s.VideoTickDur()
	abytes := 240 // bytes per synthetic audio frame
	vbytes := 700 // bytes per synthetic video access unit

	for i := 0; i < frames; i++ {
		idx := frameStart + i
		pts := s.StartPTS + int64(idx)*vDur
		dts := pts
		payload := makeVideoAU(idx, vbytes, idx%s.KeyframeEvery == 0)
		w.WritePES(PIDVideo, pts, dts, payload, false)
	}
	// PES timestamps are always 90 kHz, so one 1024-sample AAC frame is
	// 1920 ticks at 48 kHz.
	atick := int64(AudioSamplesPerFrame) * VideoTimescale / AudioTimescale
	for i := 0; i < audioFrames; i++ {
		idx := audioStart + i
		pts := s.AudioStartPTS + int64(idx)*atick
		payload := makeAudioFrame(idx, abytes)
		w.WritePES(PIDAudio, pts, pts, payload, false)
	}
	return w.Bytes()
}

// makeVideoAU builds an Annex-B access unit with an IDR or non-IDR slice.
func makeVideoAU(index, length int, key bool) []byte {
	if length < 16 {
		length = 16
	}
	b := make([]byte, 0, length)
	b = append(b, 0, 0, 0, 1, 0x67, 0x42, 0x00, 0x1E) // SPS
	b = append(b, 0, 0, 0, 1, 0x68, 0xCE, 0x38, 0x80) // PPS
	nal := byte(0x41)                                 // non-IDR slice
	if key {
		nal = 0x65 // IDR
	}
	b = append(b, 0, 0, 0, 1, nal)
	body := PayloadFor("video", index, length-13)
	b = append(b, body...)
	return b
}

// makeAudioFrame builds a synthetic AAC-like frame with ADTS sync so it is
// recognisable but not a real decoder payload.
func makeAudioFrame(index, length int) []byte {
	if length < 7 {
		length = 7
	}
	hdr := []byte{0xFF, 0xF1, 0x4C, 0x80, 0x23, 0x9F, 0xFC}
	b := append([]byte{}, hdr...)
	b = append(b, PayloadFor("audio", index, length-len(hdr))...)
	return b
}

// WriteTime is the reference epoch used for deterministic program date-time.
var WriteTime = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// FormatError describes a fixture mismatch with its actual values.
func FormatError(format string, args ...any) error { return fmt.Errorf(format, args...) }
