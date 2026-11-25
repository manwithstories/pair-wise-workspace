package fixture

import (
	pionrtp "github.com/pion/rtp"

	"mediaarchive/internal/demux"
	"mediaarchive/internal/model"
)

// samplesPerChunk controls fragmentation: every chunk holds this many samples
// per track, so checkpoints can commit real mid-file boundaries.
const samplesPerChunk = 6

// synthesize fills per-track samples with deterministic payloads. Samples are
// spread across chunk tables so the output is a fragmented MP4/MOV; clock
// anomalies and RTP gaps are synthesized from explicit rules.
func (b *builder) synthesize() error {
	// Count media (non-hint) tracks so the target bit rate is split only
	// across tracks whose payload will actually be remuxed.
	mediaCount := 0
	for _, ts := range b.spec.Tracks {
		if ts.Kind != demux.KindHint {
			mediaCount++
		}
	}
	if b.spec.MediaOnlyCount > 0 {
		mediaCount = b.spec.MediaOnlyCount
	}
	for _, ts := range b.spec.Tracks {
		var sharePermille int64
		switch {
		case ts.Kind == demux.KindHint:
			sharePermille = 0
		case mediaCount == 1:
			sharePermille = 1000
		case ts.Kind == demux.KindVideo:
			sharePermille = 800
		default:
			sharePermille = 200
		}
		step := ts.Step
		if step == 0 {
			step = 3000
			if ts.Timescale == 48000 {
				step = 1600
			}
		}
		// Exact integer byte accumulator: cumulative target bytes minus
		// emitted bytes, so per-sample rounding never accumulates into a
		// bit-rate error beyond one sample payload.
		var cumTarget, cumBytes int64
		rtpCount := uint32(0)
		for i := 0; i < ts.Samples; i++ {
			dur := step
			if ts.VFR && i%2 == 1 {
				dur = step + step/5 // VFR: alternate longer duration
			}
			// Real VFR timestamps advance by each sample's actual duration,
			// not a fixed step: DTS = start + sum of prior durations.
			var priorUnits int64
			if ts.VFR {
				priorUnits = vfrUnits(i-1, step) // durations of samples before i
				if i == 0 {
					priorUnits = 0
				}
			} else {
				priorUnits = int64(i) * int64(step)
			}
			base := ts.StartTime + uint64(priorUnits)
			rawDTS := base
			if ts.WrapUnits > 0 {
				rawDTS = base % uint64(ts.WrapUnits)
			}
			// Media duration excludes the non-zero start offset: bit rate is
			// payload over media time, so byte budgeting starts at zero even
			// though rawDTS carries StartTime for timeline alignment.
			units := int64(i+1) * int64(step)
			if ts.VFR {
				units = vfrUnits(i, step)
			}
			// target bytes against drift-corrected media time:
			//   bytes = bps/8 * units/timescale * (1e6-ppm)/1e6 * permille/1000
			// Divide at each step to keep every intermediate within int64.
			// Hint tracks are inspected for RTP gaps, never remuxed, so their
			// packet size is fixed and excluded from the bit-rate budget.
			var payLen int
			if ts.Kind == demux.KindHint {
				payLen = 64
			} else {
				rateBytes := b.spec.TargetBPS / 8
				cumTarget = rateBytes * units / int64(ts.Timescale) *
					int64(1_000_000-ts.DriftPPM) / 1_000_000 *
					sharePermille / 1000
				payLen = int(cumTarget - cumBytes)
				if payLen < 1 {
					payLen = 1
				}
				cumBytes += int64(payLen)
			}
			payload := make([]byte, payLen)
			b.rng.Read(payload)
			chunkSeq := uint32(i / samplesPerChunk)
			for len(b.tables) <= int(chunkSeq) {
				b.tables = append(b.tables, nil)
			}
			s := demux.Sample{
				TrackID:  ts.ID,
				Seq:      uint32(i + 1),
				ChunkSeq: chunkSeq,
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
			b.tables[chunkSeq] = append(b.tables[chunkSeq], sampleRec{Sample: s, payload: payload})
		}
		b.tracks = append(b.tracks, demux.TrackMeta{
			ID: ts.ID, Kind: ts.Kind, Timescale: ts.Timescale, StartTime: ts.StartTime,
			DriftPPM: ts.DriftPPM, WrapUnits: ts.WrapUnits, RTPCount: rtpCount,
			CodecHint: ts.Codec,
		})
	}
	return nil
}

// vfrUnits returns cumulative duration (in time units) of samples 0..i where
// odd samples last step+step/5 instead of step. A negative i yields zero.
func vfrUnits(i int, step uint32) int64 {
	if i < 0 {
		return 0
	}
	longs := (i + 1) / 2
	shorts := (i + 1) - longs
	return int64(shorts)*int64(step) + int64(longs)*int64(step+step/5)
}
