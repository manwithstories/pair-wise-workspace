package inspect

import (
	"sort"

	"relayaudit/internal/clock"
)

// Scanner walks record payloads in submission order and identifies frames.
//
// Each track keeps its own wrap-aware cursor, seeded from the first raw PTS on
// that track. Cursors report absolute extended ticks, so values stay comparable
// across tracks and keep advancing through the 32-bit wrap.
type Scanner struct {
	frames       []Frame
	lastAbs      map[uint8]uint64
	firstAbs     map[uint8]uint64
	keyIntervals map[uint8][]uint64
	lastIDR      map[uint8]uint64
	hasIDR       map[uint8]bool
	wraps        map[uint8]int
	order        []uint8
}

// NewScanner returns an empty scanner.
func NewScanner() *Scanner {
	return &Scanner{
		lastAbs:      map[uint8]uint64{},
		firstAbs:     map[uint8]uint64{},
		keyIntervals: map[uint8][]uint64{},
		lastIDR:      map[uint8]uint64{},
		hasIDR:       map[uint8]bool{},
		wraps:        map[uint8]int{},
	}
}

// Push inspects one record payload. absOff is the payload's byte offset in the
// source file so that every frame reports original coordinates.
func (s *Scanner) Push(track uint8, pts clock.Stamp, payload []byte, absOff int64) Frame {
	var tick uint64
	if prev, ok := s.lastAbs[track]; ok {
		abs, wrapped := clock.Extend(prev, pts)
		if wrapped {
			s.wraps[track]++
		}
		tick = abs
		s.lastAbs[track] = abs
	} else {
		tick = uint64(pts)
		s.lastAbs[track] = tick
		s.firstAbs[track] = tick
		s.order = append(s.order, track)
	}
	fr := classify(track, payload, absOff, tick)
	if fr.Type == FrameIDR {
		if s.hasIDR[track] {
			s.keyIntervals[track] = append(s.keyIntervals[track], clock.Interval(s.lastIDR[track], tick))
		}
		s.lastIDR[track] = tick
		s.hasIDR[track] = true
	}
	s.frames = append(s.frames, fr)
	return fr
}

// WrapCount returns the total number of 32-bit wraps observed.
func (s *Scanner) WrapCount() int {
	n := 0
	for _, v := range s.wraps {
		n += v
	}
	return n
}

// classify decodes the leading frame of a payload.
func classify(track uint8, payload []byte, absOff int64, tick uint64) Frame {
	fr := Frame{Track: track, Offset: absOff, Ticks: tick, Size: len(payload)}
	if hz, ok := ADTSHeaderHz(payload); ok {
		fr.Type = FrameAAC
		fr.ADTSHz = hz
		return fr
	}
	nalType, hasSPS, hasPPS, ok := AnnexBFrames(payload)
	if !ok {
		fr.Type = FrameUnknown
		return fr
	}
	fr.NalType = nalType
	fr.SPSPPS = hasSPS && hasPPS
	if nalType == nalIDR {
		fr.Type = FrameIDR
	} else {
		fr.Type = FrameNonIDR
	}
	return fr
}

// AnnexBFrames scans an Annex B byte stream for the first VCL NAL and reports
// whether parameter sets preceded it.
func AnnexBFrames(p []byte) (nalType uint8, sps, pps, ok bool) {
	for i := 0; i+3 < len(p); {
		if !(p[i] == 0 && p[i+1] == 0 && (p[i+2] == 1 || (p[i+2] == 0 && i+3 < len(p) && p[i+3] == 1))) {
			i++
			continue
		}
		var hdr int
		if p[i+2] == 1 {
			hdr = i + 3
		} else {
			hdr = i + 4
		}
		if hdr >= len(p) {
			break
		}
		t := p[hdr] & 0x1f
		switch t {
		case nalSPS:
			sps = true
		case nalPPS:
			pps = true
		case 1, 2, 3, 4, 5:
			return t, sps, pps, true
		}
		i = hdr + 1
	}
	return 0, sps, pps, false
}

// ADTSHeaderHz validates an ADTS header and returns its sample rate.
func ADTSHeaderHz(p []byte) (int, bool) {
	if len(p) < 7 || p[0] != 0xFF || p[1]&0xF0 != 0xF0 {
		return 0, false
	}
	if p[1]&0x06 != 0 { // layer must be 0 for AAC
		return 0, false
	}
	sf := int((p[2] >> 2) & 0x0f)
	rates := [...]int{96000, 88200, 64000, 48000, 44100, 32000, 24000, 22050,
		16000, 12000, 11025, 8000, 7350, 0, 0, 0}
	if rates[sf] == 0 {
		return 0, false
	}
	return rates[sf], true
}

// ADTSLength returns the declared frame length of an ADTS header.
func ADTSLength(p []byte) (int, bool) {
	if _, ok := ADTSHeaderHz(p); !ok {
		return 0, false
	}
	n := (int(p[3]&0x03) << 11) | (int(p[4]) << 3) | (int(p[5]) >> 5)
	if n < 7 {
		return 0, false
	}
	return n, true
}

// Frames returns the identified frames in push order.
func (s *Scanner) Frames() []Frame { return s.frames }

// Tracks returns the distinct tracks seen, ascending.
func (s *Scanner) Tracks() []uint8 {
	seen := map[uint8]struct{}{}
	for _, f := range s.frames {
		seen[f.Track] = struct{}{}
	}
	out := make([]uint8, 0, len(seen))
	for t := range seen {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// KeyIntervals returns the IDR-to-IDR tick distances for track.
func (s *Scanner) KeyIntervals(track uint8) []uint64 { return s.keyIntervals[track] }

// MeanKeyInterval returns the rounded mean IDR interval for track, or zero when
// fewer than two IDRs were observed.
func (s *Scanner) MeanKeyInterval(track uint8) uint64 {
	iv := s.keyIntervals[track]
	if len(iv) == 0 {
		return 0
	}
	var sum uint64
	for _, v := range iv {
		sum += v
	}
	return sum / uint64(len(iv))
}
