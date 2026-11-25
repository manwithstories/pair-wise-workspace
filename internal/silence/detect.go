// Package silence detects silent intervals inside access units on the
// unified timeline. Payloads are treated as little-endian int16 samples;
// a unit whose RMS stays under the threshold is silent, and adjacent
// silent units merge into one interval.
package silence

import (
	"encoding/binary"
	"math"

	"mediad/internal/clock"
	"mediad/internal/codec"
)

// Interval is a closed-open silent range on one track.
type Interval struct {
	Track string     `json:"track"`
	Start clock.Time `json:"start"`
	End   clock.Time `json:"end"`
}

// RMS returns the root-mean-square amplitude of int16 LE samples (0..1).
// header bytes (e.g. the 1-byte Opus TOC) are skipped so samples align.
func RMS(payload []byte, header int) float64 {
	if header > len(payload) {
		header = len(payload)
	}
	payload = payload[header:]
	n := len(payload) / 2
	if n == 0 {
		return 0
	}
	var acc float64
	for i := 0; i+1 < len(payload); i += 2 {
		s := float64(int16(binary.LittleEndian.Uint16(payload[i:]))) / 32768
		acc += s * s
	}
	return math.Sqrt(acc / float64(n))
}

// Detect finds silent intervals in one track's units. Zero-duration units
// are ignored (they carry no audio). header is the codec header size in
// bytes (1 for Opus TOC, 0 for AAC).
func Detect(trackID string, units []codec.AccessUnit, threshold float64, header int) []Interval {
	var out []Interval
	var open *Interval
	for _, u := range units {
		silent := u.Dur.Ticks > 0 && RMS(u.Payload, header) < threshold
		switch {
		case silent && open == nil:
			open = &Interval{Track: trackID, Start: u.Start, End: u.End()}
		case silent && open != nil:
			open.End = u.End()
		case !silent && open != nil:
			out = append(out, *open)
			open = nil
		}
	}
	if open != nil {
		out = append(out, *open)
	}
	return out
}

// DetectAll runs Detect over every track of a segment set.
func DetectAll(units map[string][]codec.AccessUnit, threshold float64, header int) []Interval {
	var out []Interval
	for track, us := range units {
		out = append(out, Detect(track, us, threshold, header)...)
	}
	return out
}
