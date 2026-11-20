// Package clock defines the shared media time base. Every track's 32-bit
// RTP clock is extended to a monotonic 64-bit value and, when an RTCP
// Sender Report is available, anchored to wall-clock nanoseconds so all
// tracks land on one timeline. Timestamp wrap-around is corrected; jumps
// that wrap cannot explain are rejected.
package clock

import (
	"archive/internal/model"
)

const (
	wrap32   = int64(1) << 32
	halfWrap = int64(1) << 31
	nanosPer = int64(1_000_000_000)
)

// Rescale converts v from a source timescale to a destination timescale,
// rounding to the nearest tick. Zero timescales are rejected.
func Rescale(v int64, src, dst uint32) (int64, error) {
	if src == 0 || dst == 0 {
		return 0, model.Newf(model.KindClock, "rescale", "zero timescale (src=%d dst=%d)", src, dst)
	}
	num := v * int64(dst)
	den := int64(src)
	if num >= 0 {
		return (num + den/2) / den, nil
	}
	return -((-num + den/2) / den), nil
}

// Unwrapper extends a wrapped 32-bit timestamp sequence to monotonic
// 64-bit values. A forward wrap (raw value drops by ~2^32) is corrected; a
// backward jump that wrap cannot explain is rejected.
type Unwrapper struct {
	cycles int64
	prev   int64
	init   bool
}

// Unwrap extends one raw 32-bit timestamp.
func (u *Unwrapper) Unwrap(v uint32) (int64, error) {
	ext := u.cycles + int64(v)
	if u.init {
		if ext+halfWrap < u.prev {
			u.cycles += wrap32
			ext += wrap32
		} else if ext < u.prev {
			return 0, model.Newf(model.KindClock, "unwrap",
				"non-monotonic timestamp %d after %d", ext, u.prev)
		}
	}
	u.init = true
	u.prev = ext
	return ext, nil
}

// Mapper maps one SSRC's RTP timestamps onto the shared nanosecond
// timeline. The first usable Sender Report anchors RTP time to wall clock;
// without one the stream is still mapped (first packet is time zero) but
// MissingReport stays true so the gap is reported rather than hidden.
//
// Sender-report RTP timestamps and packet timestamps are unwrapped with
// independent unwrappers: a report can legitimately reference any clock
// position, so sharing one unwrapper would corrupt the packet stream's
// monotonic state.
type Mapper struct {
	SSRC         uint32
	clockRate    uint32
	packetUW     Unwrapper
	reportUW     Unwrapper
	anchorRTP    int64 // unwrapped packet RTP ticks at the anchor
	anchorNanos  int64 // shared-timeline nanos at the anchor
	anchored     bool
	reports      int // all SRs seen, including corrupt ones
	validReports int // well-formed SRs that could anchor
}

// NewMapper creates a mapper for an SSRC with a known RTP clock rate.
func NewMapper(ssrc uint32, clockRate uint32) *Mapper {
	if clockRate == 0 {
		clockRate = 90000
	}
	return &Mapper{SSRC: ssrc, clockRate: clockRate}
}

// ApplySR folds in an RTCP Sender Report. The first valid report anchors
// RTP time to wall clock; later reports are counted but cannot move an
// already-established anchor (a moving anchor would make previously mapped
// timestamps non-monotonic). Invalid reports are counted but never anchor.
func (m *Mapper) ApplySR(sr model.SenderReport) {
	if sr.SSRC != m.SSRC {
		return
	}
	m.reports++
	if !sr.Valid {
		return
	}
	m.validReports++
	if m.anchored {
		return
	}
	ext, err := m.reportUW.Unwrap(sr.RTPTime)
	if err != nil {
		return
	}
	m.anchorRTP = ext
	m.anchorNanos = model.NTPToNanos(sr.NTPTime)
	m.anchored = true
}

// ToNanos maps a raw RTP packet timestamp to shared-timeline nanoseconds.
// The first call without a Sender Report self-anchors at time zero and is
// reported through MissingReport.
func (m *Mapper) ToNanos(rtpTS uint32) (int64, error) {
	if m.clockRate == 0 {
		return 0, model.Newf(model.KindClock, "ssrc", "zero clock rate for ssrc=%d", m.SSRC)
	}
	ext, err := m.packetUW.Unwrap(rtpTS)
	if err != nil {
		return 0, err
	}
	if !m.anchored {
		// No usable RTCP report: define a self-consistent relative origin.
		m.anchorRTP = ext
		m.anchorNanos = 0
		m.anchored = true
	}
	deltaTicks := ext - m.anchorRTP
	// Scale first, then divide to avoid truncating small tick deltas (e.g.
	// 160 audio ticks) down to zero.
	nanos := m.anchorNanos + deltaTicks*nanosPer/int64(m.clockRate)
	return nanos, nil
}

// ReportsReceived is the number of RTCP Sender Reports seen (valid or not).
func (m *Mapper) ReportsReceived() int { return m.reports }

// MissingReport reports whether mapping proceeded without any valid RTCP
// Sender Report anchor.
// MissingReport reports whether no well-formed RTCP Sender Report ever
// arrived. A corrupt report still counts toward ReportsReceived but does
// not clear this flag, since it cannot anchor the clock.
func (m *Mapper) MissingReport() bool { return m.validReports == 0 }

// ClockRate returns the configured RTP clock rate.
func (m *Mapper) ClockRate() uint32 { return m.clockRate }
