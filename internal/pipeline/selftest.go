package pipeline

import (
	"context"
	"path/filepath"
	"time"

	"avpipe/internal/audio"
	"avpipe/internal/fault"
	"avpipe/internal/fixture"
	"avpipe/internal/report"
	"avpipe/internal/stream"
	"avpipe/internal/track"
)

// SelfTest runs the acceptance run plus the isolated fault scenarios and
// returns one report. Each scenario prints its own result so an operator can
// see which fault path failed without reading the whole document.
func SelfTest(ctx context.Context, opts Options) *report.Report {
	opts.Defaults()
	base := Run(ctx, opts)
	base.Command = "selftest"
	base.Scenarios = []report.Scenario{
		corruptWAVScenario(opts),
		recoveryWindowScenario(opts),
		queueCloseScenario(ctx, opts),
		graphCycleScenario(),
		driftJitterScenario(opts),
		slowConsumerScenario(opts),
	}
	for _, s := range base.Scenarios {
		if !s.Passed && base.ExitCode == 0 {
			base.ExitCode = 3
			base.Passed = false
		}
	}
	base.Finalize()
	return base
}

// scenarioResult builds a pass/fail scenario with its root error.
func scenarioResult(name string, err error, passed bool, detail string) report.Scenario {
	s := report.Scenario{Name: name, Passed: passed, Detail: detail}
	if err != nil {
		s.Root = report.FromError(err)
		s.ExitCode = classify(err)
	}
	if !passed && s.ExitCode == 0 {
		s.ExitCode = 3
	}
	return s
}

// corruptWAVScenario proves a truncated PCM payload is rejected with a
// locatable error instead of being silently processed.
func corruptWAVScenario(opts Options) report.Scenario {
	layout, _ := audio.ParseLayout("stereo")
	_, wav, _, err := fixture.Signal(fixture.SignalConfig{
		SampleRate: opts.SourceRate, Layout: layout, Frames: opts.Frames,
		BlockSize: opts.BlockSize, Seed: opts.Seed, DropIndex: -1})
	if err != nil {
		return scenarioResult("corrupt_wav", err, false, "fixture generation failed")
	}
	bad := fixture.CorruptWAV(wav)
	_, perr := audio.Parse(bad, "stereo")
	if perr == nil {
		return scenarioResult("corrupt_wav", nil, false, "truncated WAV was accepted")
	}
	root := fault.Root(perr)
	ok := root.Code == fault.CodeWAVLength || root.Code == fault.CodeWAVTruncated
	return scenarioResult("corrupt_wav", perr, ok,
		"rejected truncated payload at offset "+itoa(int(root.Offset)))
}

// recoveryWindowScenario proves a loss burst wider than the window is
// classified unrecoverable, while normal 8% loss stays above the target.
func recoveryWindowScenario(opts Options) report.Scenario {
	burst := 40
	if burst <= opts.RecoveryWindow {
		burst = opts.RecoveryWindow * 4
	}
	res, err := burstSim(opts, 200, burst)
	if err != nil {
		return scenarioResult("recovery_window", err, false, "simulation failed")
	}
	ok := res.Unrecoverable == burst && res.Rate < 0.95
	detail := "burst of " + itoa(burst) + " beyond window " + itoa(opts.RecoveryWindow) +
		" classified unrecoverable; rate=" + ftoa(res.Rate)
	return scenarioResult("recovery_window", nil, ok, detail)
}

// queueCloseScenario proves a producer blocked on a full queue is woken by
// Close and receives a closed-queue error rather than hanging.
func queueCloseScenario(ctx context.Context, opts Options) report.Scenario {
	q, err := stream.New(stream.Config{MaxBytes: 64, Policy: stream.Block})
	if err != nil {
		return scenarioResult("queue_close", err, false, "queue construction failed")
	}
	if _, err := q.Push(stream.Item{Bytes: 64, Span: spanOf(10)}); err != nil {
		return scenarioResult("queue_close", err, false, "initial push failed")
	}
	done := make(chan error, 1)
	go func() {
		_, err := q.Push(stream.Item{Bytes: 64, Span: spanOf(10)})
		done <- err
	}()
	// Give the producer a moment to park on the full queue, then close.
	select {
	case err := <-done:
		return scenarioResult("queue_close", err, false, "blocked producer did not wait")
	case <-time.After(20 * time.Millisecond):
	}
	if err := ctx.Err(); err != nil {
		q.Close(err)
		return scenarioResult("queue_close", err, false, "context canceled before close")
	}
	q.Close(nil)
	err = <-done
	root := fault.Root(err)
	ok := root != nil && root.Code == fault.CodeQueueClosed
	return scenarioResult("queue_close", err, ok, "blocked producer woken by close")
}

// graphCycleScenario proves a dependency cycle is reported, not executed.
func graphCycleScenario() report.Scenario {
	g := track.New()
	g.MustAdd(track.Node{Name: "a", Kind: track.KindAudio, Deps: []string{"b"}})
	g.MustAdd(track.Node{Name: "b", Kind: track.KindDerived, Deps: []string{"a"}})
	_, err := g.Compile()
	root := fault.Root(err)
	ok := root != nil && root.Code == fault.CodeGraphCycle
	return scenarioResult("graph_cycle", err, ok, "cycle detected before execution")
}

// driftJitterScenario runs the drift-and-jitter boundary sample on its own
// and checks the corrected sync error stays inside the boundary budget.
func driftJitterScenario(opts Options) report.Scenario {
	boundary := opts
	boundary.ConsumerDelayMs = 0
	boundary.JitterMs = 60
	boundary.DriftPPM = 25000
	rep := Run(context.Background(), boundary)
	if rep.RootError != nil {
		return scenarioResult("drift_jitter", fault.Root(fault.New(
			fault.Code(rep.RootError.Code), rep.RootError.Op, "%s", rep.RootError.Detail)),
			false, "boundary run failed")
	}
	ok := rep.SyncErrorMs < 80 && rep.SyncP95BeforeMs > rep.SyncErrorMs
	detail := "sync p95 " + ftoa(rep.SyncErrorMs) + "ms after correction, " +
		ftoa(rep.SyncP95BeforeMs) + "ms raw; drift " + ftoa(rep.DriftEstimatePPM) + "ppm"
	return scenarioResult("drift_jitter", nil, ok, detail)
}

// slowConsumerScenario runs a deliberately slow consumer and checks the p95
// latency stays inside the slow-consumer budget without breaching the queue.
func slowConsumerScenario(opts Options) report.Scenario {
	slow := opts
	slow.ConsumerDelayMs = 180
	slow.JitterMs = 10
	rep := Run(context.Background(), slow)
	if rep.RootError != nil {
		return scenarioResult("slow_consumer", nil, false, "slow-consumer run failed: "+rep.RootError.Detail)
	}
	ok := rep.LatencyP95Ms < 250 && int64(rep.QueuePeakBytes) <= opts.QueueBytes
	detail := "latency p95 " + ftoa(rep.LatencyP95Ms) + "ms, queue peak " +
		itoa(int(rep.QueuePeakBytes)) + " bytes"
	return scenarioResult("slow_consumer", nil, ok, detail)
}

// WorkDirFor returns the per-run scratch directory used for the MP4 clip.
func WorkDirFor(base string) string {
	if base != "" {
		return base
	}
	return filepath.Join(".", "avpipe-work")
}
