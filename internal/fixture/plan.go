package fixture

import (
	"encoding/binary"

	"github.com/pion/rtp"

	"mediaslice/media"
)

// BuildPlan computes every video/audio sample and the RTP sidecar for a
// deterministic VFR timeline whose segment boundaries are jittered by at
// most BoundaryJitterTicks and compensated inside the following segment.
func BuildPlan(s Spec) *Plan {
	p := &Plan{Spec: s}
	step := media.Ticks(media.ClockRate) / media.Ticks(s.VideoRate) // 3600 @25fps
	segLen := int(s.SegmentTicks / step)                            // samples per segment
	nSeg := int(s.DurationTicks / s.SegmentTicks)
	rng := rngBytes(s.JitterSeed)

	// 1. Video samples: each segment is laid out at constant nominal step;
	// the final sample absorbs the boundary jitter so the next boundary can
	// differ from the ideal grid by at most +/-1 tick.
	cur := media.Ticks(0)
	mdatOff := uint32(0)
	idx := 0
	for seg := 0; seg < nSeg; seg++ {
		jitter := media.Ticks(0)
		if s.BoundaryJitterTicks > 0 {
			jitter = media.Ticks((rng.Intn(2*s.BoundaryJitterTicks+1) - s.BoundaryJitterTicks))
		}
		idealEnd := media.Ticks(seg+1) * s.SegmentTicks + jitter
		if seg == nSeg-1 {
			idealEnd = s.DurationTicks
		}
		segStart := cur
		for k := 0; k < segLen; k++ {
			dur := step
			isLast := k == segLen-1
			if isLast {
				dur = idealEnd - cur
				if dur <= 0 {
					dur = step
				}
			}
			key := false
			if s.KeyframeEveryN > 0 && seg%s.KeyframeEveryN == 0 && k == 0 {
				key = true
			}
			p.Video = append(p.Video, VideoSample{
				Index:      idx,
				Offset:     mdatOff,
				Size:       uint32(s.VideoSampleSize),
				StartTick:  cur,
				Duration:   dur,
				IsKeyframe: key,
			})
			mdatOff += uint32(s.VideoSampleSize)
			cur += dur
			idx++
		}
		_ = segStart
	}

	// 2. Audio samples at a constant rate in their own timescale.
	aStep := uint32(1) // one AudioRate-unit per sample => 12 kHz sample rate
	aStart := uint32(0)
	aIdx := 0
	for media.Ticks(aStart)*media.Ticks(media.ClockRate)/media.Ticks(s.AudioRate) < s.DurationTicks {
		p.Audio = append(p.Audio, AudioSample{
			Index:    aIdx,
			Offset:   mdatOff,
			Size:     uint32(s.AudioSampleSize),
			StartRaw: aStart,
			Duration: aStep,
		})
		mdatOff += uint32(s.AudioSampleSize)
		aStart += aStep
		aIdx++
	}
	p.MdatPayloadSize = int(mdatOff)
	p.MdatSize = 8 + int(mdatOff)

	// 3. RTP sidecar: timestamps start near the 32-bit boundary (guaranteeing
	// a wrap), drift linearly by RTPPPM and are truncated to 32 bits. All
	// packets share one contiguous buffer to avoid per-packet heap objects.
	driftPerTick := float64(s.RTPPPM) / 1_000_000
	pkt := &rtp.Packet{Header: rtp.Header{
		Version: 2, PayloadType: 96, SSRC: uint32(0x4d534c43 + uint32(s.Seed)),
	}}
	pkt.Payload = make([]byte, 4)
	for i, v := range p.Video {
		drift := int64(float64(v.StartTick) * driftPerTick)
		ts64 := int64(s.RTPStart) + int64(v.StartTick) + drift
		pkt.Header.SequenceNumber = uint16(i)
		pkt.Header.Timestamp = uint32(ts64)
		pkt.Payload[0], pkt.Payload[1] = byte(i>>24), byte(i>>16)
		pkt.Payload[2], pkt.Payload[3] = byte(i>>8), byte(i)
		raw, err := pkt.Marshal()
		if err != nil {
			panic(err)
		}
		p.RTPBuf = append(p.RTPBuf, raw...)
	}
	return p
}

// RTPPacketSize is the fixed on-wire size of every generated packet
// (12-byte header + 4-byte payload, no CSRC or extensions).
const RTPPacketSize = 16

// RTPTimestamp reconstructs the raw 32-bit timestamp the sidecar carries for
// video sample i (used by tests to assert wrap behavior).
func (p *Plan) RTPTimestamp(i int) uint32 {
	off := i*RTPPacketSize + 4
	return binary.BigEndian.Uint32(p.RTPBuf[off : off+4])
}

// RTPSequence reconstructs the RTP sequence number for video sample i.
func (p *Plan) RTPSequence(i int) uint16 {
	off := i*RTPPacketSize + 2
	return binary.BigEndian.Uint16(p.RTPBuf[off : off+2])
}

// MediaDuration90k returns the audio timeline duration expressed in 90 kHz
// ticks (integer rounded).
func audioDuration90k(rate uint32, raw uint32) media.Ticks {
	return media.Ticks(uint64(raw) * uint64(media.ClockRate) / uint64(rate))
}

var _ = binary.BigEndian
