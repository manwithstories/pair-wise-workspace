package pipeline

import (
	"encoding/binary"
	"hash/fnv"

	"mediakernel/internal/clock"
	"mediakernel/internal/model"
	"mediakernel/internal/recovery"
)

// packetBlock is the audio spanning one RTP packet slot, used for frame
// alignment.
type packetBlock struct {
	start clock.Ticks
	block *model.AudioBlock
}

// splitPackets slices the recovered timeline back into per-packet blocks,
// carrying origin marks forward so alignment never hides provenance.
func (e *Engine) splitPackets(full *model.AudioBlock) []packetBlock {
	stride := e.cfg.FramesPerPacket * e.cfg.Channels
	if stride == 0 || len(full.Samples) == 0 {
		return nil
	}
	out := make([]packetBlock, 0, len(full.Samples)/stride+1)
	for off := 0; off < len(full.Samples); off += stride {
		end := off + stride
		if end > len(full.Samples) {
			end = len(full.Samples)
		}
		pb := &model.AudioBlock{
			Stream:     full.Stream,
			Start:      full.Start + clock.Ticks(off/e.cfg.Channels),
			Samples:    append([]int16(nil), full.Samples[off:end]...),
			Origins:    append([]model.Origin(nil), full.Origins[off:end]...),
			Channels:   full.Channels,
			SampleRate: full.SampleRate,
		}
		out = append(out, packetBlock{start: pb.Start, block: pb})
	}
	return out
}

// alignVideo applies timestamp degradation policy and pairs each frame with
// the temporally nearest packet block.
func (e *Engine) alignVideo(frames []model.VideoFrame, blocks []packetBlock) []model.AlignedFrame {
	out := make([]model.AlignedFrame, 0, len(frames))
	for _, f := range frames {
		pts, degraded, reason := e.aligner.ValidateVideo(&f)
		var nearest *model.AudioBlock
		if len(blocks) > 0 {
			idx := nearestIndex(blocks, pts)
			nearest = blocks[idx].block
		}
		aligned := e.aligner.Align(f, pts, degraded, reason, nearest)
		out = append(out, aligned)
	}
	return out
}

// nearestIndex returns the block whose start is closest in time.
func nearestIndex(blocks []packetBlock, t clock.Ticks) int {
	best, bestDist := 0, absTicks(blocks[0].start-t)
	for i := 1; i < len(blocks); i++ {
		if d := absTicks(blocks[i].start - t); d < bestDist {
			best, bestDist = i, d
		}
	}
	return best
}

func absTicks(t clock.Ticks) clock.Ticks {
	if t < 0 {
		return -t
	}
	return t
}

// assemble computes aggregate metrics and pins determinism with a result hash.
func (e *Engine) assemble(frames []model.AlignedFrame, audio *model.AudioBlock, st recovery.Stats) *Output {
	drifts := e.aligner.DriftSamples()
	m := model.Metrics{
		Scenario:             e.cfg.Scenario,
		AVSyncMSP95:          model.Percentile(drifts, 95),
		LatencyMSP95:         model.Percentile(e.latencies, 95),
		DuplicateCount:       e.tracker.Duplicates(),
		ReorderedCount:       e.window.Reordered(),
		LateReplayCount:      e.replayer.ReplayCount(),
		UnrecoverableSamples: st.Unrecoverable,
		RealSamples:          st.Real,
		RecoveredSamples:     st.Recovered,
		ConflictCount:        e.tracker.Conflicts(),
		DroppedExpired:       e.window.Evicted(),
	}
	denom := st.Recovered + st.Unrecoverable
	if denom > 0 {
		m.LossRecoveryRate = float64(st.Recovered) / float64(denom)
	} else {
		m.LossRecoveryRate = 1 // nothing needed recovery
	}
	m.ResultHash = e.resultHash(m, frames)
	return &Output{Metrics: m, Frames: frames, Audio: audio, Faults: e.log.Items()}
}

// resultHash folds every deterministic output field into one FNV-1a digest so
// two runs of the same (scenario, seed) must compare equal byte-for-byte.
func (e *Engine) resultHash(m model.Metrics, frames []model.AlignedFrame) string {
	h := fnv.New64a()
	write := func(p []byte) { h.Write(p) }
	var buf [8]byte
	putU64 := func(v uint64) {
		binaryPutU64(buf[:], v)
		write(buf[:])
	}
	write([]byte(e.cfg.Scenario))
	putU64(uint64(m.AVSyncMSP95))
	putU64(uint64(m.LatencyMSP95))
	putU64(uint64(m.DuplicateCount))
	putU64(uint64(m.ReorderedCount))
	putU64(uint64(m.LateReplayCount))
	putU64(uint64(m.UnrecoverableSamples))
	putU64(uint64(m.RecoveredSamples))
	for _, f := range frames {
		putU64(uint64(f.Frame.FrameID))
		putU64(uint64(f.Frame.PTS))
		putU64(f.Frame.ContentHash)
		if f.Audio != nil {
			putU64(uint64(len(f.Audio.Samples)))
			for _, s := range f.Audio.Samples {
				putU64(uint64(uint16(s)))
			}
		}
		if f.Degraded {
			write([]byte{1})
		} else {
			write([]byte{0})
		}
		putU64(uint64(f.DriftMS))
	}
	return fnvHex(h.Sum64())
}

func binaryPutU64(b []byte, v uint64) {
	binary.BigEndian.PutUint64(b, v)
}

func fnvHex(v uint64) string {
	const digits = "0123456789abcdef"
	out := make([]byte, 16)
	for i := 0; i < 16; i++ {
		out[15-i] = digits[v&0xF]
		v >>= 4
	}
	return string(out)
}
