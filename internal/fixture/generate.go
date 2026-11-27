// Package fixture generates fully synthetic media packets, subtitles and
// defect scenarios. No external media is ever referenced; every byte and
// timestamp is produced deterministically in Go so reruns are identical.
package fixture

import (
	"fmt"
	"math"

	"avrepair/internal/clock"
)

// Case names understood by the generator and the CLI.
const (
	CaseClean       = "clean"
	CaseRollback    = "rollback"
	CaseOverlap     = "overlap"
	CaseMetaConflict = "meta-conflict"
	CaseTruncated   = "truncated"
	CaseInterrupted = "interrupted"
)

// KnownCases lists every generator scenario in canonical order.
var KnownCases = []string{
	CaseClean, CaseRollback, CaseOverlap, CaseMetaConflict,
	CaseTruncated, CaseInterrupted,
}

// Packet is a synthetic media packet in the unified nanosecond time base.
type Packet struct {
	Track    string  `json:"track"`
	Kind     string  `json:"kind"`
	Seq      int     `json:"seq"`
	TS       int64   `json:"ts_ns"`
	Duration int64   `json:"duration_ns"`
	Bytes    int     `json:"bytes"`
	// Luma is the packet's mean luma value 0..255 (video only).
	Luma float64 `json:"luma,omitempty"`
	// Samples holds a deterministic PCM-like waveform (audio only).
	Samples []int16 `json:"samples,omitempty"`
}

// Spec controls a generated stream.
type Spec struct {
	VideoFPS    int64
	VideoFrames int
	AudioRate   int
	AudioPackets int
	VideoBytes  int // fixed payload size per video packet
	AudioBytes  int // fixed payload size per audio packet
	DurationNS  int64
}

// DefaultSpec returns the deterministic fixture geometry shared by cases.
func DefaultSpec() Spec {
	return Spec{
		VideoFPS:     30,
		VideoFrames:  240, // 8 seconds at 30 fps
		AudioRate:    48_000,
		AudioPackets: 80,
		VideoBytes:   1500,
		AudioBytes:   480,
		DurationNS:   8 * clock.Second,
	}
}

// frameNS returns the exact presentation timestamp of video frame i.
func (s Spec) frameNS(i int) int64 {
	return int64(i) * clock.Second / s.VideoFPS
}

// FrameNS is the exported form of frameNS for cross-package manifest math.
func (s Spec) FrameNS(i int) int64 { return s.frameNS(i) }

// GenerateVideo produces a deterministic video packet stream. defect, when
// non-empty, injects the named timing defect on selected frames.
func (s Spec) GenerateVideo(defect string) []Packet {
	out := make([]Packet, 0, s.VideoFrames)
	for i := 0; i < s.VideoFrames; i++ {
		ts := s.frameNS(i)
		bytes := s.VideoBytes
		switch defect {
		case CaseRollback:
			// A single bounded backward jump mid-stream.
			if i == 120 {
				ts -= 40 * clock.Millisecond
			}
		}
		// Deterministic luma: slow ramp plus a frame-rate synced ripple.
		base := 90.0 + 60.0*float64(i)/float64(s.VideoFrames)
		luma := base + 8*math.Sin(float64(i)*0.4)
		out = append(out, Packet{
			Track: "video", Kind: "video", Seq: i, TS: ts,
			Duration: clock.Second / s.VideoFPS, Bytes: bytes, Luma: luma,
		})
	}
	return out
}

// GenerateAudio produces deterministic PCM packets at a fixed cadence.
func (s Spec) GenerateAudio(defect string) []Packet {
	out := make([]Packet, 0, s.AudioPackets)
	step := s.DurationNS / int64(s.AudioPackets)
	samplesPer := s.AudioRate / s.AudioPackets * int(s.DurationNS/clock.Second)
	for i := 0; i < s.AudioPackets; i++ {
		ts := int64(i) * step
		samples := make([]int16, samplesPer)
		for k := range samples {
			// 1 kHz sine, deterministic, full-scale-ish but not clipping.
			phase := 2 * math.Pi * 1000 * float64(i*samplesPer+k) / float64(s.AudioRate)
			samples[k] = int16(20000 * math.Sin(phase))
		}
		out = append(out, Packet{
			Track: "audio", Kind: "audio", Seq: i, TS: ts,
			Duration: step, Bytes: s.AudioBytes, Samples: samples,
		})
	}
	return out
}

// SubtitleCue is a raw generated cue before reconciliation.
type SubtitleCue struct {
	StartNS int64
	EndNS   int64
	Text    string
}

// GenerateSubtitles emits one cue per second, optionally with an injected
// overlap (CaseOverlap) or timing rollback.
func (s Spec) GenerateSubtitles(defect string) []SubtitleCue {
	var cues []SubtitleCue
	for i := 0; i < int(s.DurationNS/clock.Second); i++ {
		start := int64(i) * clock.Second
		end := start + 900*clock.Millisecond
		cues = append(cues, SubtitleCue{StartNS: start, EndNS: end,
			Text: fmt.Sprintf("cue %d", i)})
	}
	switch defect {
	case CaseOverlap:
		// Extend cue 3 so it collides with cue 4's start.
		cues[3].EndNS = cues[4].StartNS + 200*clock.Millisecond
	case CaseRollback:
		cues[5].StartNS -= 30 * clock.Millisecond
	}
	return cues
}
