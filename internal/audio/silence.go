// Package audio detects silence intervals in PCM16 audio samples and maps
// them onto the shared media clock.
package audio

import (
	"encoding/binary"
	"math"

	"mediaprobe/internal/segment"
)

// Interval is a silent range on the shared media clock.
type Interval struct {
	Start, End int64
}

// Unknown is a range that cannot be classified, with the reason why.
type Unknown struct {
	Start, End int64
	Reason     string
}

// Detector classifies audio samples as silent or sounding.
type Detector struct {
	// Threshold is the full-scale-normalized RMS below which a sample
	// counts as silent (e.g. 0.001).
	Threshold float64
	// MinDuration is the shortest reported silence, in media-clock units.
	MinDuration int64
}

// NewDetector returns a detector with the given RMS threshold and minimum
// silence duration (media-clock units).
func NewDetector(threshold float64, minDuration int64) *Detector {
	return &Detector{Threshold: threshold, MinDuration: minDuration}
}

// rms computes the normalized RMS of little-endian PCM16 data.
func rms(data []byte) float64 {
	n := len(data) / 2
	if n == 0 {
		return 0
	}
	var acc float64
	for i := 0; i+1 < len(data); i += 2 {
		v := float64(int16(binary.LittleEndian.Uint16(data[i:]))) / 32768
		acc += v * v
	}
	return math.Sqrt(acc / float64(n))
}

// Analyze walks timed audio samples in order and returns silence intervals
// plus ranges that cannot be classified (sequence gaps, uncertain timeline).
func (d *Detector) Analyze(samples []segment.TimedSample) (silence []Interval, unknown []Unknown) {
	var runStart int64
	inRun := false
	flush := func(end int64) {
		if inRun && end-runStart >= d.MinDuration {
			silence = append(silence, Interval{runStart, end})
		}
		inRun = false
	}
	for _, s := range samples {
		start, end := s.MediaPTS, s.MediaPTS+s.MediaDur
		switch {
		case s.GapBefore:
			flush(start)
			unknown = append(unknown, Unknown{start, end, "sequence gap before sample"})
		case s.Uncertain:
			flush(start)
			unknown = append(unknown, Unknown{start, end, "timeline-uncertain sample"})
		case rms(s.Data) < d.Threshold:
			if !inRun {
				runStart, inRun = start, true
			}
		default:
			flush(start)
		}
		_ = end
	}
	if len(samples) > 0 {
		last := samples[len(samples)-1]
		flush(last.MediaPTS + last.MediaDur)
	}
	return silence, unknown
}
