package capture

import (
	"sort"

	"mediarig/internal/media"
)

// TrackClock maps an extended RTP timestamp to the shared NTP rational clock.
type TrackClock struct {
	SSRC      uint32
	ClockRate int
	SRExtTS   uint64
	SRNTP     media.Rational
	Spec      PayloadSpec
}

// Map returns absolute NTP seconds for an extended RTP timestamp.
func (c TrackClock) Map(extTS uint64) media.Rational {
	delta := int64(extTS - c.SRExtTS)
	return c.SRNTP.Add(media.SamplesToSeconds(delta, c.ClockRate))
}

// ClockAlignment contains one common origin and per-SSRC mappings.
type ClockAlignment struct {
	Origin media.Rational
	Clocks map[uint32]TrackClock
}

// Relative maps an extended timestamp to seconds since the earliest track
// start. All arithmetic remains rational.
func (a *ClockAlignment) Relative(ssrc uint32, extTS uint64) (media.Rational, error) {
	c, ok := a.Clocks[ssrc]
	if !ok {
		return media.Rational{}, media.Internal(media.StageClock, "no clock for SSRC %08x", ssrc)
	}
	return c.Map(extTS).Sub(a.Origin), nil
}

// AlignClocks consumes the first SR for every SSRC and selects the earliest
// mapped first-packet time as the common program origin.
func AlignClocks(asm *Assembler) (*ClockAlignment, error) {
	ca := &ClockAlignment{Clocks: map[uint32]TrackClock{}}
	type start struct {
		ssrc uint32
		t    media.Rational
	}
	var starts []start
	for ssrc, tr := range asm.Tracks {
		sr, ok := tr.SenderReport()
		if !ok {
			return nil, media.WithSSRC(
				media.Corrupt(media.StageClock, "missing RTCP sender report"), ssrc)
		}
		srExt, ok := tr.rawTSExt[sr.RTPTimestamp]
		if !ok {
			// The SR may describe a timestamp not present in a retained
			// packet; extend it using the track's monotonic timestamp state.
			if !tr.tsInitialized {
				return nil, media.WithSSRC(media.Corrupt(media.StageClock, "cannot extend SR timestamp"), ssrc)
			}
			srExt = ExtendUint32(tr.maxTimestamp, sr.RTPTimestamp, true)
		}
		tc := TrackClock{
			SSRC: ssrc, ClockRate: tr.Spec.ClockRate, SRExtTS: srExt,
			SRNTP: sr.NTPSeconds, Spec: tr.Spec,
		}
		ca.Clocks[ssrc] = tc
		if len(tr.Packets) > 0 {
			starts = append(starts, start{ssrc: ssrc, t: tc.Map(tr.Packets[0].ExtTimestamp)})
		}
	}
	if len(starts) == 0 {
		return nil, media.Corrupt(media.StageClock, "no media timestamps to align")
	}
	sort.Slice(starts, func(i, j int) bool { return starts[i].t.Cmp(starts[j].t) < 0 })
	ca.Origin = starts[0].t
	return ca, nil
}

// TrackEnd returns the rational media-time duration from the first packet to
// the end of the last packet. samplesPerPacket is used only when the caller
// knows a constant packet duration; otherwise timestamp deltas are used.
func TrackEnd(tr *Track, samplesPerPacket uint64) media.Rational {
	if len(tr.Packets) == 0 {
		return media.MustRat(0, 1)
	}
	first := tr.Packets[0]
	last := tr.Packets[len(tr.Packets)-1]
	endTS := last.ExtTimestamp + samplesPerPacket
	return media.SamplesToSeconds(int64(endTS-first.ExtTimestamp), tr.Spec.ClockRate)
}
