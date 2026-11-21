package fixture

import (
	"bytes"
	"encoding/binary"
	"fmt"

	"github.com/pion/rtp"
)

// KLV (SMPTE 405M style) triplets: 16-byte SMPTE universal keys, BER lengths,
// and value payloads. The fixture emits only structural metadata the demuxer
// needs; sample bytes are carried in opaque payload items.

// BER16 encodes an SMPTE-style long-form BER length (max 8 groups).
func ber16(n int) []byte {
	if n < 0x80 {
		return []byte{byte(n)}
	}
	var tmp []byte
	for v := n; v > 0; v >>= 8 {
		tmp = append([]byte{byte(v)}, tmp...)
	}
	return append([]byte{byte(0x80 | len(tmp))}, tmp...)
}

func klv(key []byte, value []byte) []byte {
	if len(key) != 16 {
		panic("KLV key must be 16 bytes")
	}
	return append(append(append([]byte{}, key...), ber16(len(value))...), value...)
}

func key(b byte) []byte {
	k := make([]byte, 16)
	binary.BigEndian.PutUint64(k[8:], uint64(b))
	return k
}

// The keys used by this fixture are stable private identifiers.
var (
	keyHeader    = key(0x01)
	keyTrackList = key(0x02)
	keyBody      = key(0x03)
	keyTail      = key(0x04)
	keyTrack     = key(0x10)
	keySamples   = key(0x11)
	keyMeta      = key(0x12)
)

// buildMXF serializes an MXF-like KLV stream. If corruptTail is set the final
// track's sample run claims a length that overruns the enclosing body,
// modelling a truncated packet: the two preceding tracks stay decodable and
// the parser reports ERR_KLV_LENGTH at the bad unit, isolating only the tail.
func buildMXF(tracks []TrackSpec, corruptTail bool) []byte {
	var buf bytes.Buffer
	buf.Write(klv(keyHeader, []byte("MEDIAMELD-MXF\x00\x01")))
	buf.Write(klv(keyTrackList, trackListValue(tracks)))
	var body bytes.Buffer
	limit := len(tracks)
	if corruptTail && limit > 1 {
		limit--
	}
	for i, t := range tracks {
		if i >= limit {
			break
		}
		body.Write(klv(keyTrack, trackMetaValue(t)))
		body.Write(klv(keySamples, samplesValue(t)))
	}
	if corruptTail {
		// A KLV triplet whose declared length overruns the body region.
		body.Write(keySamples)
		body.Write(ber16(1 << 20))
		body.Write([]byte{0x00, 0x01, 0x02})
	}
	buf.Write(klv(keyBody, body.Bytes()))
	buf.Write(klv(keyTail, []byte{0}))
	return buf.Bytes()
}

func trackListValue(tracks []TrackSpec) []byte {
	var out []byte
	for _, t := range tracks {
		out = append(out, be32(t.ID)...)
	}
	return out
}

func trackMetaValue(t TrackSpec) []byte {
	var b bytes.Buffer
	b.Write(be32(t.ID))
	kind := "video"
	if t.Kind == "audio" {
		kind = "audio"
	}
	fmt.Fprintf(&b, "kind=%s;timescale=%d;desc=%s;w=%d;h=%d;rot=%d;",
		kind, t.Timescale, t.Desc, t.Width, t.Height, t.Rotation)
	if t.Color != nil {
		fmt.Fprintf(&b, "color=nclx:%d:%d:%d:%t;", t.Color.Primaries, t.Color.Transfer, t.Color.Matrix, t.Color.FullRange)
	} else {
		b.WriteString("color=unknown:zzzz;")
	}
	if t.Crop != nil {
		fmt.Fprintf(&b, "crop=%d,%d,%d,%d;", t.Crop.X, t.Crop.Y, t.Crop.W, t.Crop.H)
	}
	return b.Bytes()
}

func samplesValue(t TrackSpec) []byte {
	samples := t.Samples
	var b bytes.Buffer
	b.Write(be32(uint32(len(samples))))
	for _, s := range samples {
		b.Write(be64(uint64(s.DTS)))
		b.Write(be64(uint64(s.PTS)))
		b.Write(be64(uint64(s.Dur)))
		b.Write(be32(uint32(len(s.Data))))
		b.Write(s.Data)
	}
	return b.Bytes()
}

// DeliveryPlan names the RTP receive order. It is deterministic so repeated
// runs produce byte-identical streams. Reordering is done by moving one
// packet causally late, which the timeline engine must treat as a late event.
type DeliveryPlan struct {
	Order     []int // receive order of source frame indexes
	Duplicate int   // frame index duplicated once, or -1
	Missing   int   // frame index withheld (models a lost packet), or -1
}

// PlanDelivery builds the receive order: sorted, with one frame displaced
// later by lateGap positions and optionally one duplicate/missing packet.
func PlanDelivery(n int, lateGap int) DeliveryPlan {
	order := make([]int, 0, n+1)
	const lateIdx = 20
	if lateIdx >= n {
		for i := 0; i < n; i++ {
			order = append(order, i)
		}
		return DeliveryPlan{Order: order, Duplicate: -1, Missing: -1}
	}
	// Frames are delivered in order except one, which is displaced later by
	// exactly lateGap positions (or trails the stream if that falls off the
	// end). The frame is reinserted after the frame at lateIdx+lateGap.
	insertAfter := lateIdx + lateGap
	for i := 0; i < n; i++ {
		if i == lateIdx {
			continue
		}
		order = append(order, i)
		if i == insertAfter {
			order = append(order, lateIdx)
		}
	}
	if insertAfter >= n {
		order = append(order, lateIdx)
	}
	return DeliveryPlan{Order: order, Duplicate: 30, Missing: -1}
}

// buildRTPPackets serializes RTP packets for the camera video samples.
// Payloads carry the same essence bytes as the MXF track so the merge step can
// prove byte identity.
//
// RTP timestamps advance in true-time increments converted to source ticks at
// driftPPM (a fast capture clock emits more ticks than true elapsed time), then
// are offset by a large base near the wrap point. This is the inverse of the
// calibration the receiver applies, so drift and wrap must both be undone to
// recover the container timeline.
func buildRTPPackets(samples []SampleSpec, driftPPM int64, wrapAt uint32, plan DeliveryPlan) []byte {
	var buf bytes.Buffer
	const base = int64(3_600) * 90000 // one hour of ticks
	emit := func(idx int) {
		s := samples[idx]
		tsTicks := base + s.PTS*(1_000_000+driftPPM)/1_000_000
		ts := uint32((uint64(wrapAt) + uint64(tsTicks)) & 0xFFFFFFFF)
		p := &rtp.Packet{
			Header: rtp.Header{
				Version:        2,
				PayloadType:    96,
				SequenceNumber: uint16(1000 + idx),
				Timestamp:      ts,
				SSRC:           0x0badcafe,
			},
			Payload: append([]byte(nil), s.Data...),
		}
		raw, err := p.Marshal()
		if err != nil {
			panic(err)
		}
		buf.Write(be32(uint32(len(raw))))
		buf.Write(raw)
	}
	for _, idx := range plan.Order {
		if idx == plan.Missing {
			continue
		}
		emit(idx)
		if idx == plan.Duplicate {
			emit(idx)
		}
	}
	return buf.Bytes()
}
