// Package pipeline streams scanned records through clock normalization,
// segment cutting, silence detection, fingerprint dedup and RTP recovery,
// keeping at most MaxBufferedFrames frames in memory at once.
package pipeline

import (
	"context"
	"sort"

	"medpipe/internal/audio"
	"medpipe/internal/clock"
	"medpipe/internal/fingerprint"
	"medpipe/internal/model"
	"medpipe/internal/rtp"
	"medpipe/internal/segment"
)

// MaxBufferedFrames is the hard in-memory cap required by the platform
// constraints; runners never exceed it.
const MaxBufferedFrames = 64

// Config parameterizes one deterministic run.
type Config struct {
	WindowNS      int64
	SilenceThresh int
	SilenceMin    int
	RTP           rtp.Config
	AudioStepNS   int64 // audio block spacing for AV sync alignment
}

// Metrics is the final summary printed by the demo entry point.
type Metrics struct {
	FramesIndexed          int
	FramesBufferedPeak     int
	Segments               int
	SilenceIntervals       int
	UniquePayloads         int
	ReusedRefs             int
	PacketsSent            int
	PacketsLost            int
	FramesRecovered        int
	FramesLost             int
	PacketRecoveryPct      float64
	AVSyncErrorMS          float64
	LatencyP95MS           float64
	TruncatedTailRecovered int
	UnrecoverableSeqs      []uint32
	Digest                 string
}

// Sink receives finished segments. A failing sink aborts the run while
// preserving every metric collected up to that point.
type Sink interface {
	Write(seg model.Segment) error
}

// Runner executes one streaming pass.
type Runner struct {
	cfg      Config
	cutter   *segment.Cutter
	detector *audio.Detector
	catalog  *fingerprint.Catalog
	session  *rtp.Session
	tl       *clock.Timeline
	metrics  Metrics
}

// New wires the subsystems together.
func New(cfg Config) (*Runner, error) {
	cut, err := segment.New(cfg.WindowNS)
	if err != nil {
		return nil, err
	}
	return &Runner{
		cfg:      cfg,
		cutter:   cut,
		detector: audio.NewDetector(cfg.SilenceThresh, cfg.SilenceMin),
		catalog:  fingerprint.NewCatalog(),
		session:  rtp.NewSession(cfg.RTP),
		tl:       clock.New(clock.DefaultConfig()),
	}, nil
}

// Run streams the scan output. Cancellation or sink failure returns the
// locatable error together with the partial metrics; callers must not treat
// such a run as producing complete output.
func (r *Runner) Run(ctx context.Context, frames []*model.Frame, audioBlocks []*model.AudioBlock,
	tailRecovered int, sink Sink) (*Metrics, error) {

	r.metrics.FramesIndexed = len(frames)
	r.metrics.TruncatedTailRecovered = tailRecovered

	// Phase 1: shared-clock normalization, streamed with a bounded buffer.
	normFrames, peak, err := r.normalizeFrames(ctx, frames)
	if err != nil {
		return r.snapshot(), err
	}
	r.metrics.FramesBufferedPeak = peak

	normAudio, err := r.normalizeAudio(ctx, audioBlocks)
	if err != nil {
		return r.snapshot(), err
	}

	// Phase 2: cut on the shared clock; silence never feeds the cutter.
	segs := r.cutter.Cut(normFrames, normAudio)
	silences := r.detector.Detect(normAudio)
	r.metrics.Segments = len(segs)
	r.metrics.SilenceIntervals = len(silences)

	// Phase 3: fingerprint each normalized frame and bind segment refs.
	for _, f := range normFrames {
		ref := r.catalog.Add(f)
		for i := range segs {
			for _, seq := range segs[i].VideoSeqs {
				if seq == f.Seq {
					segs[i].PayloadRefs = append(segs[i].PayloadRefs, ref)
				}
			}
		}
	}
	stats := r.catalog.Stats()
	r.metrics.UniquePayloads = stats.UniquePayload
	r.metrics.ReusedRefs = stats.ReusedRefs

	// Phase 4: emit segments; a sink failure or cancel keeps prior metrics.
	for _, seg := range segs {
		if err := ctx.Err(); err != nil {
			return r.snapshot(), model.NewError(model.ErrCancelled, "%v", err)
		}
		if sink != nil {
			if err := sink.Write(seg); err != nil {
				return r.snapshot(), model.NewError(model.ErrWrite, "%v", err)
			}
		}
	}

	// Phase 5: deterministic RTP loss simulation and ordered recovery.
	for _, f := range normFrames {
		if err := r.session.Send(f); err != nil {
			return r.snapshot(), err
		}
	}
	recovered, rtpStats, err := r.session.Receive()
	if err != nil {
		return r.snapshot(), err
	}
	r.fillRTPMetrics(rtpStats)
	r.metrics.AVSyncErrorMS = r.avSync(normFrames, normAudio)
	r.metrics.LatencyP95MS = percentileMS(rtpStats.LatencyNS, 95)
	r.metrics.UnrecoverableSeqs = append([]uint32(nil), rtpStats.UnrecoverableSeqs...)
	r.metrics.Digest = r.runDigest(recovered)
	return r.snapshot(), nil
}

// normalizeFrames pushes frames through the timeline in batches no larger
// than MaxBufferedFrames, returning the fully normalized ordered slice.
func (r *Runner) normalizeFrames(ctx context.Context, frames []*model.Frame) ([]*model.Frame, int, error) {
	out := make([]*model.Frame, 0, len(frames))
	peak := 0
	for start := 0; start < len(frames); start += MaxBufferedFrames {
		if err := ctx.Err(); err != nil {
			return nil, peak, model.NewError(model.ErrCancelled, "%v", err)
		}
		end := start + MaxBufferedFrames
		if end > len(frames) {
			end = len(frames)
		}
		if end-start > peak {
			peak = end - start
		}
		for _, f := range frames[start:end] {
			norm, err := r.tl.Normalize(clock.Video, f.Seq, f.TimestampNS)
			if err != nil {
				return nil, peak, err
			}
			cp := *f
			cp.TimestampNS = norm
			out = append(out, &cp)
		}
	}
	return out, peak, nil
}

func (r *Runner) normalizeAudio(ctx context.Context, blocks []*model.AudioBlock) ([]*model.AudioBlock, error) {
	out := make([]*model.AudioBlock, 0, len(blocks))
	for _, b := range blocks {
		if err := ctx.Err(); err != nil {
			return nil, model.NewError(model.ErrCancelled, "%v", err)
		}
		norm, err := r.tl.Normalize(clock.Audio, b.Seq, b.TimestampNS)
		if err != nil {
			return nil, err
		}
		cp := *b
		cp.TimestampNS = norm
		out = append(out, &cp)
	}
	return out, nil
}

func (r *Runner) fillRTPMetrics(s *rtp.Stats) {
	r.metrics.PacketsSent = s.PacketsSent
	r.metrics.PacketsLost = s.PacketsLost
	r.metrics.FramesRecovered = s.FramesRecovered
	r.metrics.FramesLost = s.FramesLost
	r.metrics.PacketRecoveryPct = s.RecoveryPct
}

// avSync computes the mean absolute audio-vs-video spacing in milliseconds,
// comparing audio block instants against their nearest video frame.
func (r *Runner) avSync(frames []*model.Frame, blocks []*model.AudioBlock) float64 {
	if len(frames) == 0 || len(blocks) == 0 {
		return 0
	}
	var total int64
	for _, b := range blocks {
		nearest := frames[0].TimestampNS
		best := abs64(b.TimestampNS - nearest)
		for _, f := range frames {
			if d := abs64(b.TimestampNS - f.TimestampNS); d < best {
				best, nearest = d, f.TimestampNS
			}
		}
		total += clock.AVOffset(nearest, b.TimestampNS)
	}
	return float64(total) / float64(len(blocks)) / 1e6
}

// percentileMS returns the p-th percentile latency (nearest-rank) in ms.
func percentileMS(samplesNS []int64, p float64) float64 {
	if len(samplesNS) == 0 {
		return 0
	}
	cp := append([]int64(nil), samplesNS...)
	sort.Slice(cp, func(i, j int) bool { return cp[i] < cp[j] })
	rank := int(float64(len(cp)) * p / 100)
	if rank >= len(cp) {
		rank = len(cp) - 1
	}
	return float64(cp[rank]) / 1e6
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

func (r *Runner) snapshot() *Metrics {
	cp := r.metrics
	cp.UnrecoverableSeqs = append([]uint32(nil), r.metrics.UnrecoverableSeqs...)
	return &cp
}

// runDigest folds the recovered, time-ordered frame fingerprints so two runs
// over the same input produce the same summary string.
func (r *Runner) runDigest(recovered []*model.Frame) string {
	var joined []byte
	for _, f := range recovered {
		joined = append(joined, fingerprint.Of(f.Payload)...)
		joined = append(joined, '|')
	}
	return fingerprint.Of(joined)
}
