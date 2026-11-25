package fixture

import (
	pionrtp "github.com/pion/rtp"

	"mediaarchive/internal/demux"
	"mediaarchive/internal/model"
)

// synthesize fills per-track samples with deterministic payloads. One chunk
// (non-fragmented) keeps fixtures simple; the anomaly case exercises clock
// logic rather than fragmentation.
func (b *builder) synthesize() error {
	b.tables = [][]sampleRec{{}}
	videoShare := 0.8
	for _, ts := range b.spec.Tracks {
		share := videoShare
		if ts.Kind != demux.KindVideo {
			share = 1 - videoShare
		}
		step := uint32(3000)
		if ts.Timescale == 48000 {
			step = 1600
		}
		rtpCount := uint32(0)
		for i := 0; i < ts.Samples; i++ {
			dur := step
			if ts.VFR && i%2 == 1 {
				dur = step + step/5 // VFR: alternate longer duration
			}
			rawDTS := uint64(i) * uint64(step)
			if ts.WrapUnits > 0 {
				rawDTS %= uint64(ts.WrapUnits)
			}
			durSec := float64(dur) / float64(ts.Timescale)
			payLen := int(float64(b.spec.TargetBPS) * share / 8 * durSec)
			if payLen < 16 {
				payLen = 16
			}
			payload := make([]byte, payLen)
			b.rng.Read(payload)
			s := demux.Sample{
				TrackID:  ts.ID,
				Seq:      uint32(i + 1),
				ChunkSeq: 0,
				DTS:      rawDTS,
				Duration: dur,
				Size:     uint32(payLen),
			}
			if ts.Kind == demux.KindHint {
				seq := uint16(i)
				if ts.RTPGap && i >= 12 {
					seq++ // drop sequence number 12 -> gap
				}
				pkt := &pionrtp.Packet{
					Header: pionrtp.Header{
						Version:        2,
						PayloadType:    96,
						SequenceNumber: seq,
						SSRC:           0xa110ce,
						Timestamp:      uint32(rawDTS),
					},
					Payload: payload,
				}
				raw, err := pkt.Marshal()
				if err != nil {
					return model.NewError(model.StageCLI, "rtp marshal failed").Wrap(err)
				}
				payload = raw
				s.Payload = payload
				s.Size = uint32(len(raw))
				s.Flags |= demux.FlagRTP
				s.RTPSeq = uint32(seq)
				rtpCount++
			}
			b.tables[0] = append(b.tables[0], sampleRec{Sample: s, payload: payload})
		}
		b.tracks = append(b.tracks, demux.TrackMeta{
			ID: ts.ID, Kind: ts.Kind, Timescale: ts.Timescale, StartTime: ts.StartTime,
			DriftPPM: ts.DriftPPM, WrapUnits: ts.WrapUnits, RTPCount: rtpCount,
			CodecHint: ts.Codec,
		})
	}
	return nil
}
