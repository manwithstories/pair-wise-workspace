package pipeline

import (
	"context"

	"avpipe/internal/adapter"
	"avpipe/internal/audio"
	"avpipe/internal/media"
)

// processedAudio is one stereo, 16 kHz, normalized PCM block aligned to a
// frame sequence number.
type processedAudio struct {
	sequence uint32
	pts90k   uint64
	samples  []int16
}

// runAudio applies resample -> downmix -> normalize to every parsed block.
// Blocks whose aligned frame is corrupt are counted as gaps and skipped, so
// frame defects propagate into audio metrics.
func (e *Engine) runAudio(ctx context.Context, parsed *adapter.ParseResult) ([]processedAudio, int, error) {
	corrupt := make(map[uint32]bool, countBad(parsed.Frames))
	for _, f := range parsed.Frames {
		if f.Corrupt {
			corrupt[f.Sequence] = true
		}
	}
	down := audio.NewDownmixer()
	norm := audio.NewNormalizer(e.cfg.TargetLUFS)
	out := make([]processedAudio, 0, len(parsed.Audio))
	gaps := 0
	for _, ah := range parsed.Audio {
		if err := ctx.Err(); err != nil {
			return nil, 0, media.NewError("pipeline", media.KindCancelled, 0,
				media.ActionCancel, "%v", err)
		}
		if corrupt[ah.Sequence] {
			gaps++
			continue
		}
		resampler, err := audio.NewResampler(ah.SampleRate, 16_000)
		if err != nil {
			return nil, 0, err
		}
		channels := ah.Layout.Channels()
		rateOut := resampler.Process(ah.Samples, channels)
		stereo := down.Process(rateOut, ah.Layout)
		final := norm.Process(stereo)
		if err := e.checkCache(int64(len(final)) * 2); err != nil {
			return nil, 0, err
		}
		out = append(out, processedAudio{
			sequence: ah.Sequence,
			pts90k:   ah.PTS90k,
			samples:  final,
		})
	}
	return out, gaps, nil
}

// syncMax returns the largest frame/audio PTS difference in milliseconds.
// Audio and frame blocks share the 90 kHz clock and the same sequence anchor,
// so a healthy aligned stream reports near-zero sync error.
func (e *Engine) syncMax(parsed *adapter.ParseResult, audioOut []processedAudio) float64 {
	ptsBySeq := make(map[uint32]uint64, len(parsed.Frames))
	for _, f := range parsed.Frames {
		if !f.Corrupt {
			ptsBySeq[f.Sequence] = f.PTS90k
		}
	}
	var maxMillis uint64
	for _, a := range audioOut {
		if framePTS, ok := ptsBySeq[a.sequence]; ok {
			if d := media.SyncDiffMillis(framePTS, a.pts90k); d > maxMillis {
				maxMillis = d
			}
		}
	}
	return float64(maxMillis)
}
