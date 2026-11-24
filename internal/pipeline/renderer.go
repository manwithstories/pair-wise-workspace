package pipeline

import (
	"path/filepath"

	"mediasim/internal/clock"
	"mediasim/internal/fixture"
	"mediasim/internal/mp4"
	"mediasim/internal/render"
)

// renderOutcome is what the frame loop produced.
type renderOutcome struct {
	frames    int
	hashes    []render.FrameHash
	sync      []float64
	latencies []float64
}

// renderFrames composites every delivered frame whose origin bytes are
// intact, in ascending output order. A frame whose origin is truncated or
// corrupt is skipped with a classified error instead of being invented.
func renderFrames(outDir string, seed int64, cfg Config, info *mp4.Info, rec *mp4.Recovery,
	transport transportResult, errs *clock.List) (renderOutcome, error) {
	var out renderOutcome
	writer, err := render.NewWriter(filepath.Join(outDir, "frames"), render.DefaultParams(seed))
	if err != nil {
		return out, err
	}
	var syncSamples, latencies []float64
	sched := schedule()
	for _, idx := range transport.Delivered {
		// A frame whose origin bytes were truncated or corrupted cannot be
		// rendered honestly: mark the range and skip it. This is how damage
		// in the container propagates through transport into the output.
		if !localFrameAvailable(info, rec, idx) {
			src, sidx := frameSample(idx)
			sp := clock.NewSpan("video", src,
				clock.Interval{Start: int64(idx) * fixture.VideoFrameTicks,
					End: int64(idx+1) * fixture.VideoFrameTicks})
			sp.Frame = idx
			errs.Add(clock.Errorf(clock.CatSample, sp, clock.RecoveredSkipped,
				"frame %d unavailable in origin sample %d", idx, sidx))
			continue
		}
		sf := sched[idx]
		block, drift, err := audioBlockAt(sf.Ticks)
		if err != nil {
			errs.Add(clock.WithSpan(err, clock.NewSpan("voice", "voice0",
				clock.Interval{Start: sf.Ticks, End: sf.Ticks + fixture.VideoFrameTicks})))
			continue
		}
		sf.Audio = block
		// Report the full capture-to-commit path for this frame: network
		// queueing, jitter target, and any repair from the local origin.
		latency := transport.LatencyMs[idx]
		if _, repaired := transport.repaired[idx]; repaired {
			// A repaired frame waits for the local origin instead of the
			// network: half the jitter target, not the full warning time.
			latency += float64(cfg.JitterMs) / 4
		}
		layers := []render.Layer{
			{Kind: render.LayerVideo, Source: sf.CamSource, Frame: sf.CamFrame, Weight: 1},
			{Kind: render.LayerMarker, Label: "tactical", Frame: int64(idx)},
		}
		if sf.Crossfade {
			layers = append(layers, render.Layer{Kind: render.LayerTransition, Label: "xfade", Frame: int64(idx)})
		}
		frame := render.Frame{Index: idx, Ticks: sf.Ticks, Layers: layers, DriftMs: drift}
		if _, err := writer.Write(frame); err != nil {
			errs.Add(err)
			continue
		}
		syncSamples = append(syncSamples, drift)
		latencies = append(latencies, latency)
	}
	out.frames = writer.Count()
	out.hashes = writer.Hashes()
	out.sync = syncSamples
	out.latencies = latencies
	return out, nil
}
