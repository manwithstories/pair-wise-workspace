package pipeline

import (
	"avcheck/internal/clock"
	"avcheck/internal/export"
	"avcheck/internal/flow"
	"avcheck/internal/metrics"
	"avcheck/internal/render"
	"avcheck/internal/timeline"
)

// finalize mixes the recovered tracks, renders frames, writes the MP4 and
// builds the acceptance report. Sync failures are captured as a tick count
// rather than aborting, so the CLI can still print the full metric line.
func finalize(p Params, stages map[uint32]*stage,
	allBlocks []*clock.Block, queue *flow.Queue,
	lat *metrics.Latency, latePackets int64) (*Result, error) {
	cfg := p.Fixture
	tracks := make([]timeline.Track, 0, len(stages))
	var syncTicks int64
	for id := uint32(1); int(id) <= cfg.Tracks; id++ {
		st, ok := stages[id]
		if !ok {
			continue
		}
		blocks := st.reasm.Emitted() // full recovered timeline for this track
		tracks = append(tracks, timeline.Track{
			ID: id, Channels: cfg.Channels, Blocks: blocks,
		})
	}
	_ = allBlocks

	mix, err := timeline.Mix(tracks)
	if err != nil {
		if ce, ok := err.(*clock.Error); ok && ce.Kind == clock.KindSync {
			syncTicks = 1 // surfaced as sync_error_samples in the report
		} else {
			return nil, err
		}
	}

	res := &Result{
		Queue:          queue.Stats(),
		LatePackets:    latePackets,
		AcceptedBlocks: queue.Stats().Enqueued,
		SyncErrorTicks: syncTicks,
	}
	for _, st := range stages {
		s := st.reasm.Stats()
		res.Recovered.Interpolated += s.Interpolated
		res.Recovered.SilenceFilled += s.SilenceFilled
	}

	if mix == nil {
		res.Report = metrics.Build(syncTicks, res.Recovered, res.Queue, lat,
			0, 0, nil, latePackets)
		return res, nil
	}

	seq := render.NewRenderer(clock.SampleRate).Render(mix)
	if p.Out != nil {
		exp := export.NewExporter(clock.SampleRate, cfg.Channels)
		if _, err := exp.Write(p.Out, seq, mix.Samples, mix.Channels); err != nil {
			return nil, err
		}
	}
	res.Report = metrics.Build(syncTicks, res.Recovered, res.Queue, lat,
		len(seq.Frames), seq.FrameHash, mix.PerTrackFrames, latePackets)
	return res, nil
}
