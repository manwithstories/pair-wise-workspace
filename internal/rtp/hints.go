// Package rtp parses RTP packets carried inside MOV hint samples. Sequence
// gaps and malformed packets are located by track id, sample seq and byte
// offset and reported through the unified error model; gaps are recoverable,
// unparseable packets are not.
package rtp

import (
	"fmt"

	pionrtp "github.com/pion/rtp"

	"mediaarchive/internal/demux"
	"mediaarchive/internal/model"
)

// Gap is one detected RTP sequence discontinuity.
type Gap struct {
	TrackID   uint32
	SampleSeq uint32
	Offset    int64
	Expected  uint16
	Got       uint16
	Missing   int
}

// Inspector is stateful per hint track: it remembers the last RTP sequence
// number so gap detection works across chunks.
type Inspector struct {
	lastSeq uint16
	hasLast bool
	ssrc    uint32
	hasSSRC bool
	// packets counts successfully parsed packets.
	packets int
}

// Inspect parses one hint sample's RTP payload. A sequence gap returns a
// recoverable model.Error and a Gap; the packet itself is still accepted.
func (in *Inspector) Inspect(s demux.Sample) ([]Gap, error) {
	var pkt pionrtp.Packet
	if err := pkt.Unmarshal(s.Payload); err != nil {
		return nil, model.NewError(model.StageRTP,
			fmt.Sprintf("malformed RTP packet on track %d sample %d", s.TrackID, s.Seq)).
			WithTrack(int32(s.TrackID)).WithOffset(s.Offset).WithSample(s.Seq).Wrap(err)
	}
	in.packets++
	if !in.hasSSRC {
		in.ssrc = pkt.SSRC
		in.hasSSRC = true
	} else if pkt.SSRC != in.ssrc {
		return nil, model.NewError(model.StageRTP,
			fmt.Sprintf("SSRC changed mid-track: %08x -> %08x", in.ssrc, pkt.SSRC)).
			WithTrack(int32(s.TrackID)).WithOffset(s.Offset).WithSample(s.Seq)
	}
	var gaps []Gap
	if in.hasLast {
		want := in.lastSeq + 1
		if pkt.SequenceNumber != want {
			missing := int(pkt.SequenceNumber) - int(want)
			if missing < 0 {
				missing += 1 << 16 // handle 16-bit rollover
			}
			gaps = append(gaps, Gap{
				TrackID:   s.TrackID,
				SampleSeq: s.Seq,
				Offset:    s.Offset,
				Expected:  want,
				Got:       pkt.SequenceNumber,
				Missing:   missing,
			})
		}
	}
	in.lastSeq = pkt.SequenceNumber
	in.hasLast = true
	return gaps, nil
}

// Packets returns the number of successfully parsed packets.
func (in *Inspector) Packets() int { return in.packets }

// AsError turns a gap into a recoverable, locatable pipeline error.
func (g Gap) AsError() *model.Error {
	return model.NewError(model.StageRTP,
		fmt.Sprintf("RTP sequence gap on track %d sample %d: expected %d got %d (%d missing)",
			g.TrackID, g.SampleSeq, g.Expected, g.Got, g.Missing)).
		WithTrack(int32(g.TrackID)).WithOffset(g.Offset).WithSample(g.SampleSeq).Recover()
}

// InspectTrack walks all hint samples of one track through one inspector and
// collects every gap. It stops only on unrecoverable parse failures.
func InspectTrack(in *Inspector, samples []demux.Sample) ([]Gap, error) {
	var all []Gap
	for _, s := range samples {
		gaps, err := in.Inspect(s)
		if err != nil {
			return nil, err
		}
		all = append(all, gaps...)
	}
	return all, nil
}
