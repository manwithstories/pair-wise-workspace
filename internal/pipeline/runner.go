package pipeline

import (
	"context"
	"os"
	"path/filepath"

	"mediamix/internal/audio"
	"mediamix/internal/buffer"
	"mediamix/internal/clock"
	"mediamix/internal/rtp"
	"mediamix/internal/txn"
)

// Runner executes one end-to-end processing run over a generated fixture.
type Runner struct {
	cfg Config
}

// New builds a runner after validating its configuration.
func New(cfg Config) (*Runner, error) {
	if cfg.QueueBytes <= 0 || cfg.QueueBytes > buffer.MaxQueueBytes {
		return nil, clock.NewError(clock.StageBuffer, clock.ErrInvalidParam, 0, false,
			"queue byte budget out of range")
	}
	if cfg.QueueItems <= 0 {
		return nil, clock.NewError(clock.StageBuffer, clock.ErrInvalidParam, 0, false,
			"queue item capacity must be positive")
	}
	return &Runner{cfg: cfg}, nil
}

// Result is the outcome of a run, consumed by the metrics/report layer.
type Result struct {
	Output         []byte
	Commits        []txn.CommitManifest
	OutputSHA256   string
	PeakQueueBytes int
	Ingest         rtp.Stats
	Concealed      int64
	RecoveredGaps  int64
	TruncatedTails int64
	RejectedSR     int
	CalibrationOK  bool
	FaultInjected  bool
	Replayed       bool
}

// Run processes the fixture. When a fault is configured, the first attempt
// injects it at the named stage and rolls the open transaction back; the
// runner then reprocesses the whole fixture without the fault against a
// fresh working directory, yielding bytes identical to a clean run.
func (r *Runner) Run(ctx context.Context, fx *rtp.Fixture) (*Result, error) {
	if err := clock.CheckDuration(fixtureDuration(fx, r.cfg.Source)); err != nil {
		return nil, err
	}
	if r.cfg.Fault != FaultNone {
		faultyCfg := r.cfg
		faultyCfg.WorkDir = filepath.Join(r.cfg.WorkDir, "faulty")
		if err := os.MkdirAll(faultyCfg.WorkDir, 0o755); err != nil {
			return nil, clock.WrapError(clock.StageCommit, clock.ErrInvalidPath, 0, false,
				"create faulty workdir", err)
		}
		faulty, err := (&Runner{cfg: faultyCfg}).runOnce(ctx, fx, true)
		if err != nil {
			return nil, err
		}
		if !faulty.FaultInjected {
			return nil, clock.NewError(clock.StageCommit, clock.ErrFaultInjected, 0, false,
				"configured fault was never injected")
		}
		// Clean rerun in an isolated directory: same deterministic input, no
		// fault, so its bytes are the reference-quality recovered output.
		cleanCfg := r.cfg
		cleanCfg.Fault = FaultNone
		cleanCfg.WorkDir = filepath.Join(r.cfg.WorkDir, "recovered")
		if err := os.MkdirAll(cleanCfg.WorkDir, 0o755); err != nil {
			return nil, clock.WrapError(clock.StageCommit, clock.ErrInvalidPath, 0, false,
				"create recovered workdir", err)
		}
		res, err := (&Runner{cfg: cleanCfg}).runOnce(ctx, fx, false)
		if err != nil {
			return nil, clock.WrapError(clock.StageCommit, clock.ErrRollback, 0, false,
				"recovery rerun failed", err)
		}
		res.FaultInjected = true
		res.Replayed = true
		return res, nil
	}
	return r.runOnce(ctx, fx, false)
}

// runOnce performs a single pass. expectFault controls whether the configured
// fault is actually armed (the recovery rerun passes false).
func (r *Runner) runOnce(ctx context.Context, fx *rtp.Fixture, expectFault bool) (*Result, error) {
	q, err := buffer.NewWindow[Frame](r.cfg.QueueItems, r.cfg.QueueBytes, Frame.ByteSize)
	if err != nil {
		return nil, err
	}
	defer q.Close()

	committer, err := txn.Open(r.cfg.WorkDir)
	if err != nil {
		return nil, err
	}
	defer committer.Close()

	conv, err := audio.NewConverter(audio.DefaultSource(), r.cfg.Target)
	if err != nil {
		return nil, err
	}
	loud, err := audio.NewLoudnessNormaliser(r.cfg.Target.SampleRate, r.cfg.Target.Channels)
	if err != nil {
		return nil, err
	}
	defer loud.Close()
	plc := audio.NewPLC(audio.DefaultPLCConfig())
	cal := rtp.NewCalibrator(r.cfg.Source.ClockRate)
	reorder := rtp.NewReorderer(r.cfg.Reorder)

	res := &Result{}
	consumerErr := make(chan error, 1)
	go func() {
		consumerErr <- r.consume(ctx, q, conv, loud, plc, committer, cal, res, expectFault)
	}()

	if err := r.produce(ctx, fx, reorder, cal, q); err != nil {
		q.Close()
		<-consumerErr
		return nil, err
	}
	q.Close()
	cerr := <-consumerErr
	// A fault injection is an expected stop for the faulty pass.
	if cerr != nil && !(expectFault && clock.IsCode(cerr, clock.ErrFaultInjected)) {
		return nil, cerr
	}

	// On the faulty pass we stop early; no final flush or output rebuild.
	if expectFault && res.FaultInjected {
		res.Ingest = reorder.Stats()
		res.PeakQueueBytes = q.PeakBytes()
		res.RejectedSR = cal.Rejected
		return res, nil
	}

	tail, err := loud.Flush()
	if err != nil {
		return nil, err
	}
	if len(tail) > 0 {
		if _, err := committer.Stage("loudness", f32Bytes(tail)); err != nil {
			return nil, err
		}
		if _, err := committer.Commit("loudness"); err != nil {
			return nil, err
		}
	}

	manifests, err := committer.Replay()
	if err != nil {
		return nil, clock.WrapError(clock.StageCommit, clock.ErrReplay, 0, false, "final replay", err)
	}
	output, err := r.rebuildOutput(committer)
	if err != nil {
		return nil, err
	}
	res.Commits = manifests
	res.Output = output
	res.OutputSHA256 = sha256Hex(output)
	res.Ingest = reorder.Stats()
	res.Concealed = plc.ConcealedFrames()
	res.RecoveredGaps = plc.ConcealedSpans()
	res.TruncatedTails = plc.TruncatedTails()
	res.RejectedSR = cal.Rejected
	res.CalibrationOK = cal.Valid()
	res.PeakQueueBytes = q.PeakBytes()
	return res, nil
}

// produce pushes every generated packet through the reorderer and enqueues
// ordered frames. Sender Reports calibrate the shared timeline.
func (r *Runner) produce(ctx context.Context, fx *rtp.Fixture, reorder *rtp.Reorderer,
	cal *rtp.Calibrator, q *buffer.Window[Frame]) error {
	for _, sr := range fx.Reports {
		_ = cal.Apply(sr) // rejected reports are counted inside the calibrator
	}
	enqueue := func(p *rtp.Packet) error {
		tick, _ := cal.ToTick(p.Timestamp)
		fr := Frame{Packet: p, Tick: int64(tick), Seq: p.Seq, Payload: p.Payload}
		return q.Put(ctx, fr)
	}
	for _, pkt := range fx.Packets {
		ordered, _ := reorder.Push(pkt)
		for _, p := range ordered {
			if err := enqueue(p); err != nil {
				return err
			}
		}
	}
	for _, p := range reorder.Flush() {
		if err := enqueue(p); err != nil {
			return err
		}
	}
	return nil
}
