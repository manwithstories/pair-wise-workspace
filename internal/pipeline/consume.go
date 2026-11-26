package pipeline

import (
	"context"
	"encoding/binary"
	"math"
	"time"

	"mediamix/internal/audio"
	"mediamix/internal/buffer"
	"mediamix/internal/clock"
	"mediamix/internal/rtp"
	"mediamix/internal/txn"
)

// consume drains ordered frames, applying conversion, PLC, loudness and
// per-fragment transactional commits. Injected faults trigger a rollback to
// the last commit; the fragment is then reprocessed cleanly on rerun.
func (r *Runner) consume(ctx context.Context, q *buffer.Window[Frame],
	conv *audio.Converter, loud *audio.LoudnessNormaliser, plc *audio.PLC,
	committer *txn.Commit, cal *rtp.Calibrator, res *Result, expectFault bool) error {

	pts := int64(0)

	for {
		fr, err := q.Get(ctx)
		if clock.IsCode(err, clock.ErrQueueClosed) {
			return nil
		}
		if err != nil {
			return err
		}
		if r.cfg.SlowConsumerMs > 0 {
			select {
			case <-time.After(time.Duration(r.cfg.SlowConsumerMs) * time.Millisecond):
			case <-ctx.Done():
				return ctx.Err()
			}
		}

		// A Lost frame occupies the exact sequence position of a dropped
		// packet. Fill it with deterministic PLC at the target rate, then run
		// the same loudness graph and commit boundary as genuine audio.
		if fr.Lost {
			if err := r.fillGap(loud, plc, committer, 1); err != nil {
				return err
			}
			continue
		}

		// Stage 1: resample/remix the real fragment.
		converted, err := conv.Convert(fr.Payload)
		if err != nil {
			return err
		}

		// Injected faults discard the uncommitted fragment and stop the run.
		// Only the armed (faulty) pass injects; the recovery rerun passes
		// expectFault=false and processes cleanly.
		if expectFault && r.cfg.Fault != FaultNone && !res.FaultInjected {
			res.FaultInjected = true
			_ = committer.Rollback("injected fault: " + string(r.cfg.Fault))
			return clock.NewError(clock.StageCommit, clock.ErrFaultInjected, fr.Seq, true,
				"fault injected "+string(r.cfg.Fault))
		}

		norm, err := loud.Process(converted, pts)
		if err != nil {
			return err
		}
		pts += int64(len(converted) / r.cfg.Target.Channels)
		plc.Observe(converted)

		if len(norm) > 0 {
			if _, err := committer.Stage("loudness", f32Bytes(norm)); err != nil {
				return err
			}
			if _, err := committer.Commit("loudness"); err != nil {
				return err
			}
		}
	}
}

// fillGap synthesises missing packets at the target sample rate. Each lost
// source packet spans PacketSpan source samples; PLC emits the equivalent
// number of target-rate stereo frames, which then traverse the same loudness
// graph and commit boundary as genuine audio.
func (r *Runner) fillGap(loud *audio.LoudnessNormaliser, plc *audio.PLC,
	committer *txn.Commit, missingPackets int) error {

	ratio := r.cfg.Target.SampleRate / int(r.cfg.Source.ClockRate)
	missingTargetFrames := missingPackets * int(r.cfg.Source.PacketSpan) * ratio
	concealed, ok, err := plc.Conceal(missingTargetFrames, false)
	if err != nil || !ok {
		if err == nil {
			err = clock.NewError(clock.StagePLC, clock.ErrGap, 0, true, "plc returned no samples")
		}
		return err
	}
	norm, err := loud.Process(concealed, 0)
	if err != nil {
		return err
	}
	if len(norm) > 0 {
		if _, err := committer.Stage("plc", f32Bytes(norm)); err != nil {
			return err
		}
		if _, err := committer.Commit("plc"); err != nil {
			return err
		}
	}
	return nil
}

// f32Bytes serialises interleaved float32 samples to little-endian bytes.
func f32Bytes(samples []float32) []byte {
	buf := make([]byte, len(samples)*4)
	for i, v := range samples {
		binary.LittleEndian.PutUint32(buf[i*4:], math.Float32bits(v))
	}
	return buf
}

func fixtureDuration(fx *rtp.Fixture, sc rtp.StreamConfig) time.Duration {
	samples := int(fx.TotalSeq) * int(sc.PacketSpan)
	secs := float64(samples) / float64(sc.ClockRate)
	return time.Duration(secs * float64(time.Second))
}
