// Package clock maps per-track RTP timestamps onto the shared
// microsecond timeline. RTCP Sender Reports anchor the mapping and a
// least-squares fit tracks sender clock drift; without reports the
// model falls back to the nominal rate with a growing error bound.
package clock

import (
	"math"

	"github.com/asticode/go-astiav"

	"archiver/internal/media"
)

const ntpUnixEpochDelta = 2208988800 // seconds between 1900 and 1970

// driftPPM bounds the assumed sender clock drift when no RTCP Sender
// Report constrains the model.
const driftPPM = 100e-6

// Anchor binds one extended RTP timestamp to an absolute wall-clock
// time in microseconds (from an RTCP Sender Report).
type Anchor struct {
	NTPUs  int64
	RTPExt uint64
}

// Model is the clock state for one track (one SSRC).
type Model struct {
	rate       int
	nominalUs  float64 // microseconds per RTP tick at the nominal rate
	anchors    []Anchor
	slope      float64 // fitted microseconds per tick
	intercept  float64
	lastExt    uint64
	hasLast    bool
	lastUs     media.TimeUs
	hasUs      bool
}

// NewModel creates a model for a track with the given RTP clock rate
// (e.g. 44100 for audio, 90000 for video).
func NewModel(rate int) *Model {
	tb := astiav.NewRational(1, rate)
	toUs := astiav.NewRational(media.TimebaseHz, 1)
	nominal := tb.Mul(toUs).Float64()
	return &Model{rate: rate, nominalUs: nominal, slope: nominal}
}

// Rate returns the RTP clock rate in Hz.
func (m *Model) Rate() int { return m.rate }

// ntpToUnixUs converts an NTP timestamp to Unix microseconds.
func ntpToUnixUs(sec, frac uint32) int64 {
	unix := int64(sec) - ntpUnixEpochDelta
	return unix*media.TimebaseHz + int64((uint64(frac)*media.TimebaseHz)>>32)
}

// extend maps a 32-bit RTP timestamp onto the 64-bit axis, choosing
// the wrap cycle nearest to the last seen value.
func (m *Model) extend(ts uint32) uint64 {
	if !m.hasLast {
		return uint64(ts)
	}
	base := m.lastExt &^ 0xffffffff
	best, bestDist := uint64(0), uint64(math.MaxUint64)
	for _, c := range [3]uint64{base, base + 1 << 32, base - 1 << 32} {
		if base == 0 && c == base-1<<32 {
			continue
		}
		cand := c + uint64(ts)
		d := cand - m.lastExt
		if cand < m.lastExt {
			d = m.lastExt - cand
		}
		if d < bestDist {
			best, bestDist = cand, d
		}
	}
	return best
}

// AddSenderReport feeds an RTCP Sender Report into the model and
// refits the drift estimate.
func (m *Model) AddSenderReport(ntpSec, ntpFrac, rtpTS uint32) {
	a := Anchor{NTPUs: ntpToUnixUs(ntpSec, ntpFrac), RTPExt: m.extend(rtpTS)}
	m.anchors = append(m.anchors, a)
	m.fit()
}

func (m *Model) fit() {
	n := len(m.anchors)
	if n == 0 {
		return
	}
	if n == 1 {
		m.slope = m.nominalUs
		m.intercept = float64(m.anchors[0].NTPUs) - m.slope*float64(m.anchors[0].RTPExt)
		return
	}
	var sx, sy, sxx, sxy float64
	for _, a := range m.anchors {
		x, y := float64(a.RTPExt), float64(a.NTPUs)
		sx += x
		sy += y
		sxx += x * x
		sxy += x * y
	}
	nf := float64(n)
	den := nf*sxx - sx*sx
	if den == 0 {
		return
	}
	m.slope = (nf*sxy - sx*sy) / den
	m.intercept = (sy - m.slope*sx) / nf
}

// ToMediaTime converts an RTP timestamp to shared-timeline
// microseconds. The result never moves backwards: once a media time
// has been emitted, later packets clamp to it. The returned bound is
// the current worst-case error in microseconds.
func (m *Model) ToMediaTime(rtpTS uint32) (media.TimeUs, media.TimeUs) {
	ext := m.extend(rtpTS)
	m.lastExt, m.hasLast = ext, true
	us := media.TimeUs(math.Round(m.slope*float64(ext) + m.intercept))
	if m.hasUs && us < m.lastUs {
		us = m.lastUs // monotonic guard
	}
	m.lastUs, m.hasUs = us, true
	return us, m.errorBound(ext)
}

// errorBound grows with the distance from the nearest anchor; with no
// anchors it grows from the first observed timestamp.
func (m *Model) errorBound(ext uint64) media.TimeUs {
	ref := ext
	if len(m.anchors) > 0 {
		ref = m.anchors[0].RTPExt
		for _, a := range m.anchors {
			if absDiff(a.RTPExt, ext) < absDiff(ref, ext) {
				ref = a.RTPExt
			}
		}
	} else if m.hasLast {
		ref = m.lastExt
	}
	dist := float64(absDiff(ref, ext))
	return media.TimeUs(1000 + dist*m.nominalUs*driftPPM)
}

func absDiff(a, b uint64) uint64 {
	if a > b {
		return a - b
	}
	return b - a
}

// Snapshot is the serializable clock state stored in checkpoints.
type Snapshot struct {
	Rate      int
	Anchors   []Anchor
	Slope     float64
	Intercept float64
	LastExt   uint64
	HasLast   bool
	LastUs    media.TimeUs
	HasUs     bool
}

func (m *Model) Snapshot() Snapshot {
	return Snapshot{
		Rate:      m.rate,
		Anchors:   append([]Anchor(nil), m.anchors...),
		Slope:     m.slope,
		Intercept: m.intercept,
		LastExt:   m.lastExt,
		HasLast:   m.hasLast,
		LastUs:    m.lastUs,
		HasUs:     m.hasUs,
	}
}

// RestoreModel rebuilds a model from a checkpoint snapshot.
func RestoreModel(s Snapshot) *Model {
	m := NewModel(s.Rate)
	m.anchors = append([]Anchor(nil), s.Anchors...)
	m.slope, m.intercept = s.Slope, s.Intercept
	m.lastExt, m.hasLast = s.LastExt, s.HasLast
	m.lastUs, m.hasUs = s.LastUs, s.HasUs
	return m
}
