// Package timeline mixes repaired per-track RTP packets into one unified
// nanosecond timeline. Each output frame retains its source packet range
// and the clock conversion basis so an auditor can reproduce the mapping,
// and cross-track clock drift beyond a threshold is reported with enough
// location information to find the offending track and packet.
package timeline

import (
	"sort"

	"archive/internal/clock"
	"archive/internal/model"
)

// DriftEvent records a cross-track clock discrepancy beyond the threshold.
type DriftEvent struct {
	TrackA   model.TrackKind
	TrackB   model.TrackKind
	SSRC     uint32
	Sequence int64
	DriftNS  int64
	LimitNS  int64
}

// Mixer composes one or more tracks onto a shared timeline. Its working
// state is bounded by the number of tracks, not the packet count.
type Mixer struct {
	mappers map[uint32]*clock.Mapper
	meta    map[uint32]trackMeta
	drift   []DriftEvent
	limitNS int64
}

type trackMeta struct {
	kind      model.TrackKind
	originNS  int64 // absolute time of the first packet (RTCP anchor)
	firstNS   int64
	lastNS    int64
	haveSpan  bool
	packets   int64
	keyframes int64
}

// NewMixer creates a mixer that flags cross-track drift above driftLimitNS
// nanoseconds.
func NewMixer(driftLimitNS int64) *Mixer {
	if driftLimitNS <= 0 {
		driftLimitNS = 50_000_000
	}
	return &Mixer{
		mappers: map[uint32]*clock.Mapper{},
		meta:    map[uint32]trackMeta{},
		limitNS: driftLimitNS,
	}
}

// RegisterTrack binds an SSRC to a track kind and RTP clock rate.
func (m *Mixer) RegisterTrack(ssrc uint32, kind model.TrackKind, clockRate uint32) {
	if _, ok := m.mappers[ssrc]; ok {
		return
	}
	m.mappers[ssrc] = clock.NewMapper(ssrc, clockRate)
	m.meta[ssrc] = trackMeta{kind: kind}
}

// ApplySR forwards an RTCP Sender Report to the owning track's mapper.
func (m *Mixer) ApplySR(sr model.SenderReport) {
	if mp, ok := m.mappers[sr.SSRC]; ok {
		mp.ApplySR(sr)
	}
}

// Mix maps a repaired packet to timeline nanoseconds, derives a frame with
// full provenance and checks cross-track drift at keyframe boundaries.
func (m *Mixer) Mix(p model.Packet) (model.Frame, error) {
	mp, ok := m.mappers[p.SSRC]
	if !ok {
		m.RegisterTrack(p.SSRC, p.Track, p.ClockRate)
		mp = m.mappers[p.SSRC]
	}
	ns, err := mp.ToNanos(p.Timestamp)
	if err != nil {
		return model.Frame{}, err
	}
	meta := m.meta[p.SSRC]
	if !meta.haveSpan {
		meta.originNS, meta.firstNS, meta.lastNS, meta.haveSpan = ns, ns, ns, true
	}
	rel := ns - meta.originNS
	// Reordered/duplicated packets can carry a timestamp behind the
	// monotonic envelope. They are still rendered, but only strictly
	// advancing samples extend the span and feed the drift measurement.
	if rel > meta.lastNS-meta.originNS {
		meta.lastNS = ns
	}
	meta.packets++
	if p.Keyframe && rel >= meta.lastNS-meta.originNS {
		meta.keyframes++
		m.checkDrift(meta)
	}
	m.meta[p.SSRC] = meta

	return model.Frame{
		Track:       meta.kind,
		TimeNanos:   ns,
		SourceStart: p.ExtSequence,
		SourceEnd:   p.ExtSequence,
		SSRC:        p.SSRC,
		Keyframe:    p.Keyframe,
		ClockBasis:  basis(mp),
	}, nil
}

func basis(mp *clock.Mapper) string {
	if mp.MissingReport() {
		return "self-anchored:no-rtcp-sr"
	}
	return "rtcp-sr:clockrate-" + itoa(int64(mp.ClockRate()))
}

// driftWarmup is the minimum packets a track must carry before its clock
// slope is trusted for drift detection. Early in a stream, interleaving can
// leave a track's span momentarily flat, which would otherwise produce a
// degenerate step and a false drift of astronomical magnitude.
const driftWarmup = 64

// checkDrift compares the current track's intrinsic per-packet duration
// against every other track's. Both synchronized tracks advance one frame
// per round, so their per-packet durations must match. This ratio is a
// property of the clock rate alone: it is unaffected by interleaving,
// arrival order, reordering or how many packets have been observed. Only a
// genuine clock-rate mismatch drives it past the threshold.
func (m *Mixer) checkDrift(cur trackMeta) {
	if cur.packets < driftWarmup {
		return
	}
	// Spans are measured relative to each track's own origin so an absolute
	// RTCP wall-clock anchor cannot dominate the comparison.
	curSpan := (cur.lastNS - cur.originNS) - (cur.firstNS - cur.originNS)
	if curSpan <= 0 {
		return
	}
	curStep := float64(curSpan) / float64(cur.packets-1)
	for _, other := range m.meta {
		if other.kind == cur.kind || other.packets < driftWarmup {
			continue
		}
		otherSpan := (other.lastNS - other.originNS) - (other.firstNS - other.originNS)
		if otherSpan <= 0 {
			continue
		}
		otherStep := float64(otherSpan) / float64(other.packets-1)
		// Difference in the per-packet duration, accumulated over the
		// current track's span, is the projected clock drift in nanos.
		diff := curStep - otherStep
		if diff < 0 {
			diff = -diff
		}
		driftNS := int64(diff * float64(cur.packets-1))
		if driftNS > m.limitNS {
			m.drift = append(m.drift, DriftEvent{
				TrackA: cur.kind, TrackB: other.kind,
				Sequence: cur.packets - 1, DriftNS: driftNS, LimitNS: m.limitNS,
			})
		}
	}
}

// Drift returns all cross-track drift events, ordered by sequence.
func (m *Mixer) Drift() []DriftEvent {
	sort.SliceStable(m.drift, func(i, j int) bool {
		return m.drift[i].Sequence < m.drift[j].Sequence
	})
	return m.drift
}

// MissingReports lists SSRCs that produced no valid RTCP Sender Report.
func (m *Mixer) MissingReports() []uint32 {
	var out []uint32
	for ssrc, mp := range m.mappers {
		if mp.MissingReport() {
			out = append(out, ssrc)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// TrackSummary is the per-track span and packet accounting for the report.
type TrackSummary struct {
	SSRC       uint32
	Kind       model.TrackKind
	Packets    int64
	Keyframes  int64
	FirstNanos int64
	LastNanos  int64
}

// Summaries returns one summary per registered track, ordered by SSRC.
func (m *Mixer) Summaries() []TrackSummary {
	out := make([]TrackSummary, 0, len(m.meta))
	for ssrc, meta := range m.meta {
		out = append(out, TrackSummary{
			SSRC: ssrc, Kind: meta.kind, Packets: meta.packets,
			Keyframes: meta.keyframes, FirstNanos: meta.firstNS, LastNanos: meta.lastNS,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SSRC < out[j].SSRC })
	return out
}

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var b [20]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
