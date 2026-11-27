// Package pipeline wires the manifest, RTP, buffer, syncer, bitrate and
// report stages into one bounded, resumable recovery run. It executes the
// fixture-generated streams, repairs loss inside the finite window, aligns
// the result onto the manifest timeline and enforces the strict-mode and
// memory invariants the archive contract relies on.
package pipeline

import (
	"context"
	"fmt"

	"archive/internal/bitrate"
	"archive/internal/buffer"
	"archive/internal/fixture"
	"archive/internal/manifest"
	"archive/internal/model"
	"archive/internal/report"
	"archive/internal/rtp"
	"archive/internal/syncer"
)

// CaseKind names the three built-in recovery scenarios.
type CaseKind string

const (
	CaseNormal    CaseKind = "normal"
	CaseRepaired  CaseKind = "repaired"
	CaseDamaged   CaseKind = "damaged"
)

// Config carries the CLI-derived parameters for one run.
type Config struct {
	Kind            CaseKind
	Format          manifest.Format
	Seed            int64
	TargetKbps      int
	BandwidthKbps   []float64
	DuplicateEvery  int
	ReorderWindow   int
	LossBurst       int
	MemoryBytes     int
	Strict          bool
	// InterruptAfter, when > 0, cancels the run after this many media packets
	// and then resumes from the retained progress.
	InterruptAfter  int
}

// Run executes one recovery scenario end to end and returns its summary.
func Run(ctx context.Context, cfg Config) (report.Summary, error) {
	if err := validateConfig(cfg); err != nil {
		return report.Summary{}, err
	}
	log := model.NewFaultLog()
	params := buildParams(cfg)
	stream := fixture.Generate(params)

	tl, err := buildTimeline(cfg, params, log)
	if err != nil {
		return report.Summary{}, err
	}

	runner := &runner{
		cfg: cfg, log: log, params: params, stream: stream, tl: tl,
		queue: buffer.NewQueue[*rtp.MediaPacket](cfg.MemoryBytes, packetSize, log),
		repair: rtp.NewRepairer(log),
		jitter: rtp.NewJitterBuffer(model.MaxRepairWindow, log),
		parser: rtp.NewParser(params.ClockRate, log),
		estim:  bitrate.NewEstimator(cfg.TargetKbps, log),
	}
	return runner.execute(ctx)
}

type runner struct {
	cfg    Config
	log    *model.FaultLog
	params fixture.Params
	stream *fixture.Stream
	tl     *manifest.Timeline

	queue   *buffer.Queue[*rtp.MediaPacket]
	repair  *rtp.Repairer
	jitter  *rtp.JitterBuffer
	parser  *rtp.Parser
	estim   *bitrate.Estimator
	selector *bitrate.Selector

	packetsTotal   int
	crcMatched     int
	retrans        int
	fec            int
	aligned        []syncer.AlignedPacket
}

func validateConfig(cfg Config) error {
	if cfg.MemoryBytes <= 0 || cfg.MemoryBytes > model.MaxQueueBytes {
		return model.InvariantError(model.PhasePipeline,
			"memory limit %d outside (0,%d]", cfg.MemoryBytes, model.MaxQueueBytes)
	}
	if cfg.TargetKbps <= 0 {
		return model.InvariantError(model.PhasePipeline, "target bitrate must be positive")
	}
	if len(cfg.BandwidthKbps) == 0 {
		return model.InvariantError(model.PhasePipeline, "at least one bandwidth sample is required")
	}
	return nil
}

func buildParams(cfg Config) fixture.Params {
	p := fixture.DefaultParams(cfg.Seed)
	p.DuplicateEvery = cfg.DuplicateEvery
	p.ReorderWindow = cfg.ReorderWindow
	p.Strict = cfg.Strict
	switch cfg.Kind {
	case CaseNormal:
		// No loss, no corruption: the clean reference path.
	case CaseRepaired:
		p.LossBurst = 1
		p.LossEvery = 4 // single losses, fully covered by parity groups
	case CaseDamaged:
		p.LossBurst = cfg.LossBurst
		// A cadence that is deliberately NOT aligned to the four-packet
		// parity groups: bursts then straddle group boundaries, leaving some
		// groups with two missing members that XOR parity cannot rebuild.
		p.LossEvery = 22
	}
	return p
}

func buildTimeline(cfg Config, p fixture.Params, log *model.FaultLog) (*manifest.Timeline, error) {
	discont := -1
	if p.Segments > 1 {
		discont = 1 // discontinuity after segment index 1
	}
	playlists := fixture.BuildPlaylist(cfg.Format, p, 500, discont)
	base := "/archive/" + string(cfg.Format)
	if cfg.Format == manifest.FormatHLS {
		return manifest.ParseHLS(stringReader(playlists[manifest.FormatHLS]), base, log)
	}
	return manifest.ParseDASH(stringReader(playlists[manifest.FormatDASH]), base, log)
}

func packetSize(p *rtp.MediaPacket) int {
	if p == nil {
		return 0
	}
	return p.Size
}

// execute runs the ingest -> bounded queue -> repair -> align flow,
// including the optional interrupt/resume cycle. Ingest owns the queue's
// Close: once every datagram has been offered (or rejected), the queue is
// closed so the draining process loop can observe the end of input. This
// ordering prevents the ingest-blocks-on-full / process-waits-for-close
// deadlock, while backpressure still throttles ingest through Push.
func (r *runner) execute(ctx context.Context) (report.Summary, error) {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	ingestErr := make(chan error, 1)
	go func() {
		ingestErr <- r.ingest(runCtx, cancel)
	}()

	// The processing loop deliberately uses a context detached from the
	// interruptible ingest context: an interruption pauses the producer but
	// must never abort the consumer, which still has buffered work to drain.
	if err := r.process(context.WithoutCancel(runCtx)); err != nil {
		return report.Summary{}, err
	}
	if err := <-ingestErr; err != nil {
		return report.Summary{}, err
	}
	return r.finalize()
}

// ingest pushes datagrams through the bounded queue and closes it when done.
// The queue applies backpressure when the memory ceiling is reached; an
// optional interrupt cancels the context mid-stream and the retained items
// are then resumed on a detached context so buffered progress survives.
func (r *runner) ingest(ctx context.Context, cancel context.CancelFunc) error {
	defer r.queue.Close()
	seen := 0
	interrupted := false
	for _, pk := range r.stream.Packets {
		if r.cfg.InterruptAfter > 0 && !interrupted && seen >= r.cfg.InterruptAfter {
			cancel()
			interrupted = true
			// Detach from the cancelled context: already-queued items stay
			// available and ingestion continues, proving the interruption
			// neither loses progress nor deadlocks the pipeline.
			ctx = context.WithoutCancel(ctx)
		}
		if pk.IsParity {
			r.repair.AddGroup(rtp.NewParityGroup(pk.Group, pk.ExtSeq, pk.Payload))
			continue
		}
		mp, err := r.parser.Parse(pk.Datagram)
		if err != nil {
			return err
		}
		if err := r.queue.Push(ctx, mp); err != nil {
			return err
		}
		seen++
	}
	return nil
}

func (r *runner) finalize() (report.Summary, error) {
	aligner := syncer.NewAligner(r.tl, r.log)
	aligned, err := aligner.Finalize(r.aligned)
	if err != nil {
		return report.Summary{}, err
	}
	r.aligned = aligned

	gapCount := len(r.log.Gaps())
	complete := r.tl.SegmentCount() - segmentsWithGaps(r.tl, r.log)
	actual := r.estimateActualKbps()

	counters := report.Counters{
		TargetKbps: r.cfg.TargetKbps, ActualKbps: actual,
		PacketsTotal: r.packetsTotal, CRCMatched: r.crcMatched,
		SegmentsTotal: r.tl.SegmentCount(), SegmentsComplete: complete,
		Retransmitted: r.retrans, FECRecovered: r.fec, Gaps: gapCount,
		PeakBytes: r.peakPeak(), MemoryLimit: int64(r.cfg.MemoryBytes),
		SyncErrorNanos: int64(aligner.MaxError()),
		Backpressure:   r.queue.BackpressureEvents(),
	}
	summary, err := report.Build(fmt.Sprintf("%s-%d", r.cfg.Kind, r.cfg.Seed), counters)
	if err != nil {
		return report.Summary{}, err
	}
	if r.cfg.Strict && gapCount > 0 {
		r.log.Add(model.Fault{Phase: model.PhasePipeline, Kind: model.FaultStrictUnrecovered,
			Detail: fmt.Sprintf("%d unrecoverable gaps in strict mode", gapCount)})
		return summary, model.ErrStrictUnrecovered
	}
	return summary, nil
}
