package pipeline

import (
	"context"
	"math"

	"avpipe/internal/audio"
	"avpipe/internal/clock"
	"avpipe/internal/fault"
	"avpipe/internal/fixture"
	"avpipe/internal/rng"
	"avpipe/internal/rtp"
	"avpipe/internal/stream"
	"avpipe/internal/sync"
)

// audioResult is the outcome of the audio chain.
type audioResult struct {
	sourceFrames int64
	outputFrames int64
	dropped      int64
	evicted      int64
	latencies    []float64
	before       []audio.Block
	after        []audio.Block
	stats        audio.LoudnessStats
	roundingErr  float64
	plan         audio.MappingPlan
	format       audio.Format
}

// runAudio performs generate -> parse -> decode -> map -> resample -> queue ->
// loudness. The queue is a real bounded queue; the consumer pacing is modeled
// deterministically so the byte/duration limits and the policy are exercised
// without scheduler dependence.
func runAudio(ctx context.Context, opts Options) (audioResult, stream.Stats, error) {
	var res audioResult
	srcLayout, err := audio.ParseLayout(opts.SourceLayout)
	if err != nil {
		return res, stream.Stats{}, err
	}
	dstLayout, err := audio.ParseLayout(opts.TargetLayout)
	if err != nil {
		return res, stream.Stats{}, err
	}
	if err := audio.ValidateLoudnessConfig(audio.LoudnessConfig{
		TargetLUFS: opts.TargetLUFS, PeakCeiling: opts.PeakCeiling}); err != nil {
		return res, stream.Stats{}, err
	}
	_, wav, format, err := fixture.Signal(fixture.SignalConfig{
		SampleRate: opts.SourceRate, Layout: srcLayout, Frames: opts.Frames,
		BlockSize: opts.BlockSize, Amplitude: 0.2, NoiseAmp: 0.004,
		Seed: opts.Seed, DropIndex: -1,
	})
	if err != nil {
		return res, stream.Stats{}, fault.Wrap(fault.CodeInternal, "pipeline.audio", err, "generate input")
	}
	info, err := audio.Parse(wav, opts.SourceLayout)
	if err != nil {
		return res, stream.Stats{}, err
	}
	decoded, err := audio.Decode(wav, info, opts.BlockSize)
	if err != nil {
		return res, stream.Stats{}, err
	}
	r, err := audio.NewResampler(audio.ResamplerConfig{
		SrcRate: format.SampleRate, DstRate: opts.TargetRate, Layout: dstLayout})
	if err != nil {
		return res, stream.Stats{}, err
	}
	var emitted []audio.Block
	for _, b := range decoded {
		if err := ctx.Err(); err != nil {
			return res, stream.Stats{}, fault.Wrap(fault.CodeQueueCanceled, "pipeline.audio", err,
				"context canceled during audio stage")
		}
		mappedData, plan, err := audio.MapLayout(b.Layout, b.Data, b.Frames, dstLayout)
		if err != nil {
			return res, stream.Stats{}, err
		}
		res.plan = plan
		mb := audio.Block{StartFrame: b.StartFrame, StartTick: b.StartTick,
			SampleRate: b.SampleRate, Layout: dstLayout, Frames: b.Frames, Data: mappedData}
		if err := r.Push(mb); err != nil {
			return res, stream.Stats{}, err
		}
		emitted = append(emitted, r.Slurp(false, opts.BlockSize)...)
	}
	emitted = append(emitted, r.Slurp(true, opts.BlockSize)...)
	if len(emitted) == 0 {
		return res, stream.Stats{}, fault.New(fault.CodeInternal, "pipeline.audio",
			"audio stage produced no output blocks")
	}

	// Every block goes through a real bounded queue driven by the Pump: a
	// virtual consumer retires one block per block duration and the producer
	// pushes at each block's media arrival time. Queue depth therefore
	// reflects genuine backlog, and the policy is exercised exactly as live.
	arrivals := make([]float64, 0, len(emitted))
	src := rng.New(opts.Seed ^ 0xA5A5A5A5)
	blockMs := float64(opts.BlockSize) * 1000 / float64(opts.TargetRate)
	for i := range emitted {
		arrivals = append(arrivals, float64(i)*blockMs+src.Float()*opts.JitterMs)
	}
	q, err := stream.New(stream.Config{MaxBytes: opts.QueueBytes, MaxTicks: opts.QueueTicks,
		Policy: stream.Policy(opts.Policy)})
	if err != nil {
		return res, stream.Stats{}, err
	}
	// The consumer keeps pace with the block cadence (one block retired per
	// block duration); the configured consumer delay is added playout
	// latency. Queue depth therefore grows only from arrival bursts, which
	// is what the jitter and queue limits are meant to absorb.
	pump := &Pump{Queue: q, ServiceMs: blockMs, ConsumerMs: opts.ConsumerDelayMs}
	pr, err := pump.Run(emitted, arrivals)
	if err != nil {
		return res, q.Stats(), err
	}
	res.before = pr.Blocks
	res.dropped, res.evicted = pr.Dropped, pr.Evicted
	res.latencies = pr.Latencies
	if len(res.before) == 0 {
		return res, q.Stats(), fault.New(fault.CodeInternal, "pipeline.audio",
			"queue admitted no blocks")
	}
	res.after, res.stats = audio.Loudness(res.before, audio.LoudnessConfig{
		TargetLUFS: opts.TargetLUFS, PeakCeiling: opts.PeakCeiling})
	res.roundingErr = r.RoundingError()
	res.sourceFrames = r.InFrames()
	res.outputFrames = r.OutFrames()
	res.format = format
	return res, q.Stats(), nil
}

// runRTP sends the audio blocks as RTP packets through the lossy path. Every
// packet is marshalled and re-parsed with pion/rtp before simulation.
func runRTP(ctx context.Context, opts Options, blocks []audio.Block) (rtp.Recovery, error) {
	if err := ctx.Err(); err != nil {
		return rtp.Recovery{}, fault.Wrap(fault.CodeQueueCanceled, "pipeline.rtp", err, "canceled")
	}
	sent := make([]rtp.Packet, len(blocks))
	for i, b := range blocks {
		sent[i] = rtp.Packet{Seq: uint16(i), Timestamp: uint32(b.StartFrame),
			SSRC: 0x5EED, Payload: make([]byte, 200), Marker: i == len(blocks)-1}
	}
	for i := range sent {
		buf, err := sent[i].Marshal()
		if err != nil {
			return rtp.Recovery{}, err
		}
		if _, err := rtp.Unmarshal(buf); err != nil {
			return rtp.Recovery{}, err
		}
	}
	// Derive an independent stream so RTP loss/jitter does not repeat the
	// audio-gain jitter pattern for the same seed.
	return rtp.SimulateReceive(sent, rtp.Config{
		Seed: opts.Seed ^ 0x9E3779B9, LossRate: opts.Loss, JitterMs: opts.JitterMs,
		Window: opts.RecoveryWindow, LatePackets: len(sent) / 50, ConflictCopy: len(sent) / 100,
	})
}

// runSync aligns video frame timestamps with the audio timeline.
func runSync(opts Options, blocks []audio.Block) (sync.Result, error) {
	if len(blocks) < 2 {
		return sync.Result{}, fault.New(fault.CodeInternal, "pipeline.sync",
			"need at least 2 audio blocks, got %d", len(blocks))
	}
	step := blocks[1].StartTick - blocks[0].StartTick
	if step <= 0 {
		step = clock.FromMillis(20)
	}
	intervals := len(blocks)
	if intervals > 500 {
		intervals = 500
	}
	// The aligned video track spans the whole audio run at the negotiated
	// drift, with a handful of recovery-driven resyncs across the stream.
	// The aligned video stream uses its own derived seed so its jitter is
	// independent of the audio pacing jitter.
	return sync.Align(sync.Config{Seed: opts.Seed ^ 0x85EBCA6B, JitterMs: opts.JitterMs,
		DriftPPM: opts.DriftPPM, Recoveries: intervals / 50}, intervals, step)
}

// blockStats returns RMS and peak for interleaved sample slices.
func blockStats(blocks []audio.Block) (rms, peak float64) {
	var sum float64
	var n int
	for _, b := range blocks {
		for _, v := range b.Data {
			sum += v * v
			n++
			if a := math.Abs(v); a > peak {
				peak = a
			}
		}
	}
	if n == 0 {
		return 0, 0
	}
	return math.Sqrt(sum / float64(n)), peak
}
