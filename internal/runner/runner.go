// Package runner turns a plan into encoded segment results with resumable state.
package runner

import (
	"context"
	"fmt"
	"math"
	"sort"
	"sync"
	"time"

	"rcctl/internal/codec"
	"rcctl/internal/metrics"
	"rcctl/internal/plan"
	"rcctl/internal/ratecontrol"
	"rcctl/internal/scheduler"
	"rcctl/internal/source"
	"rcctl/internal/state"
)

type Options struct {
	Workers       int
	MaxAttempts   int
	Backoff       time.Duration
	RatePasses    int
	RateTolerance float64
	Logf          func(string, ...any)
}

// BuildPlan performs the first pass and then applies complexity-aware allocation.
func BuildPlan(ctx context.Context, p *plan.Plan) error {
	synth, err := source.New(p.Clip, p.Frames, p.Width, p.Height, p.Seed)
	if err != nil {
		return err
	}
	enc, err := codec.NewEncoder(p.Width, p.Height)
	if err != nil {
		return err
	}
	for i := range p.Segments {
		seg := &p.Segments[i]
		var total int64
		var reference *source.Frame
		for frame := seg.StartFrame; frame < seg.EndFrame; frame++ {
			if err := ctx.Err(); err != nil {
				return err
			}
			src, err := synth.At(frame)
			if err != nil {
				return err
			}
			res, err := enc.EncodeFrame(src, reference, 32, frame == seg.StartFrame)
			if err != nil {
				return err
			}
			total += int64(res.Bytes)
			recon := res.Reconstructed
			reference = &recon
		}
		seg.FirstPassKbps = plan.ActualKbps(total, seg.Frames, p.FPS)
		seg.Complexity = math.Max(1, seg.FirstPassKbps)
		seg.HardCut = source.HardCut(seg.StartFrame)
	}
	demands := make([]ratecontrol.SegmentDemand, len(p.Segments))
	for i, seg := range p.Segments {
		demands[i] = ratecontrol.SegmentDemand{Frames: seg.Frames, Complexity: seg.Complexity, HardCut: seg.HardCut}
	}
	targets := ratecontrol.Allocate(p.TargetKbps, demands)
	for i := range p.Segments {
		p.Segments[i].TargetKbps = targets[i]
	}
	return nil
}

// Execute skips journal-completed segments and retries the rest independently.
func Execute(ctx context.Context, p *plan.Plan, completed map[int]state.SegmentResult, journal *state.Journal, opts Options) ([]state.SegmentResult, map[int]bool, error) {
	if opts.Workers < 1 {
		opts.Workers = 1
	}
	if opts.MaxAttempts < 1 {
		opts.MaxAttempts = 1
	}
	if opts.RatePasses < 1 {
		opts.RatePasses = 6
	}
	if opts.RateTolerance <= 0 {
		opts.RateTolerance = 0.5
	}
	if opts.Logf == nil {
		opts.Logf = func(string, ...any) {}
	}
	synth, err := source.New(p.Clip, p.Frames, p.Width, p.Height, p.Seed)
	if err != nil {
		return nil, nil, err
	}
	enc, err := codec.NewEncoder(p.Width, p.Height)
	if err != nil {
		return nil, nil, err
	}
	var doneMu sync.Mutex
	done := make(map[int]state.SegmentResult, len(p.Segments))
	for k, v := range completed {
		done[k] = v
	}
	var tasks []scheduler.Task[state.SegmentResult]
	for i := range p.Segments {
		seg := p.Segments[i]
		if _, ok := completed[seg.Index]; ok {
			continue
		}
		segCopy := seg
		session := &segmentSession{p: p, seg: segCopy, synth: synth, enc: enc, journal: journal,
			doneMu: &doneMu, done: done, opts: opts}
		tasks = append(tasks, scheduler.Task[state.SegmentResult]{
			Index: seg.Index,
			Run: func(ctx context.Context) (state.SegmentResult, error) {
				return session.Attempt(ctx)
			},
		})
	}
	outcomes := scheduler.Run(ctx, opts.Workers, opts.MaxAttempts, opts.Backoff, tasks)
	failed := make(map[int]bool)
	for _, out := range outcomes {
		if out.Err != nil {
			failed[out.Index] = true
			opts.Logf("segment %d failed after %d attempts: %v", out.Index, out.Attempts, out.Err)
			if journal != nil {
				_ = journal.Append(state.Record{Type: state.SegmentFailed, PlanID: p.PlanID,
					Segment: out.Index, State: state.Failed, Message: out.Err.Error()})
			}
		}
	}
	results := make([]state.SegmentResult, 0, len(done))
	for _, r := range done {
		results = append(results, r)
	}
	sort.Slice(results, func(i, j int) bool { return results[i].Index < results[j].Index })
	return results, failed, nil
}

type segmentSession struct {
	p        *plan.Plan
	seg      plan.Segment
	synth    *source.Synth
	enc      *codec.Encoder
	journal  *state.Journal
	doneMu   *sync.Mutex
	done     map[int]state.SegmentResult
	opts     Options
	machine  *state.Machine
	final    state.SegmentResult
	recorded bool
	attempts int
}

func (s *segmentSession) Attempt(ctx context.Context) (state.SegmentResult, error) {
	s.attempts++
	if s.recorded {
		return s.final, nil
	}
	if s.machine == nil {
		m, err := state.NewMachine(state.Planned)
		if err != nil {
			return state.SegmentResult{}, err
		}
		s.machine = m
	}
	if s.machine.Status() == state.Done {
		return s.persistDone()
	}
	if s.machine.Status() == state.Failed || s.machine.Status() == state.Planned {
		if err := s.machine.Transition(state.Encoding); err != nil {
			return state.SegmentResult{}, err
		}
	}
	if s.journal != nil {
		if err := s.journal.Append(state.Record{Type: state.SegmentStarted, PlanID: s.p.PlanID,
			Segment: s.seg.Index, State: state.Encoding, Message: fmt.Sprintf("attempt=%d", s.attempts)}); err != nil {
			_ = s.machine.Transition(state.Failed)
			return state.SegmentResult{}, err
		}
	}
	res, err := encodeSegment(ctx, s.p, s.seg, s.synth, s.enc, s.opts)
	if err != nil {
		_ = s.machine.Transition(state.Failed)
		return state.SegmentResult{}, err
	}
	res.Attempts = s.attempts
	if err := s.machine.Transition(state.Verifying); err != nil {
		return state.SegmentResult{}, err
	}
	res.Status = state.Done
	if err := s.machine.Transition(state.Done); err != nil {
		return state.SegmentResult{}, err
	}
	s.final = res
	return s.persistDone()
}

func (s *segmentSession) persistDone() (state.SegmentResult, error) {
	if s.journal != nil {
		if err := s.journal.Append(state.Record{Type: state.SegmentDone, PlanID: s.p.PlanID,
			Segment: s.seg.Index, State: state.Done, Result: &s.final}); err != nil {
			return s.final, err
		}
	}
	s.doneMu.Lock()
	s.done[s.seg.Index] = s.final
	checkpoint := make([]state.SegmentResult, 0, len(s.done))
	for _, r := range s.done {
		checkpoint = append(checkpoint, r)
	}
	s.doneMu.Unlock()
	if s.journal != nil {
		if err := s.journal.WriteCheckpoint(s.p.PlanID, checkpoint); err != nil {
			s.opts.Logf("segment %d checkpoint warning: %v", s.seg.Index, err)
		}
	}
	s.recorded = true
	if s.final.Capped {
		s.opts.Logf("warning: segment %d hit the quantizer ceiling; target is below the codec floor", s.seg.Index)
	}
	s.opts.Logf("segment %d done: %.2f kbps (target %.2f), PSNR %.2f dB", s.seg.Index,
		s.final.ActualKbps, s.final.TargetKbps, s.final.PSNRDB)
	return s.final, nil
}

func encodeSegment(ctx context.Context, p *plan.Plan, seg plan.Segment, synth *source.Synth, enc *codec.Encoder, opts Options) (state.SegmentResult, error) {
	frames := make([]source.Frame, 0, seg.Frames)
	for frame := seg.StartFrame; frame < seg.EndFrame; frame++ {
		if err := ctx.Err(); err != nil {
			return state.SegmentResult{}, err
		}
		f, err := synth.At(frame)
		if err != nil {
			return state.SegmentResult{}, err
		}
		frames = append(frames, f)
	}
	targetBytes := float64(p.SegmentBytes(seg))
	if targetBytes < 1 {
		targetBytes = 1
	}
	firstBytes := seg.FirstPassKbps * 1000 / 8 / float64(p.FPS) * float64(seg.Frames)
	q := 32 * math.Pow(math.Max(0.05, firstBytes/targetBytes), 0.72)
	q = clamp(q, 0.5, 255)
	best := segmentAttempt{}
	bestDev := math.Inf(1)
	prevQ, prevBytes := 0.0, 0.0
	for pass := 0; pass < opts.RatePasses; pass++ {
		if err := ctx.Err(); err != nil {
			return state.SegmentResult{}, err
		}
		attempt, err := encodeAtQ(ctx, frames, enc, q, float64(seg.TargetKbps)*1000/float64(p.FPS))
		if err != nil {
			return state.SegmentResult{}, err
		}
		dev := (float64(attempt.bytes) - targetBytes) / targetBytes * 100
		if math.Abs(dev) < bestDev {
			best, bestDev = attempt, math.Abs(dev)
		}
		if math.Abs(dev) <= opts.RateTolerance {
			break
		}
		next := q * math.Pow(float64(attempt.bytes)/targetBytes, 0.72)
		if prevQ > 0 && float64(attempt.bytes) != prevBytes && q != prevQ {
			a := (math.Log(q) - math.Log(prevQ)) / (math.Log(float64(attempt.bytes)) - math.Log(prevBytes))
			if !math.IsNaN(a) && !math.IsInf(a, 0) && a > 0.1 && a < 3 {
				next = q * math.Pow(float64(attempt.bytes)/targetBytes, a)
			}
		}
		prevQ, prevBytes = q, float64(attempt.bytes)
		next = clamp(next, 0.5, 255)
		if math.Abs(next-q) < 0.01 {
			next = q * 1.03
		}
		q = next
	}
	if best.recon == nil {
		return state.SegmentResult{}, fmt.Errorf("segment %d produced no encode attempt", seg.Index)
	}
	stats := metrics.Compute(frames, best.recon)
	actual := plan.ActualKbps(best.bytes, seg.Frames, p.FPS)
	dev := (actual - seg.TargetKbps) / seg.TargetKbps * 100
	capped := best.maxQ >= 254.99 && float64(best.bytes) > targetBytes
	return state.SegmentResult{Index: seg.Index, Frames: seg.Frames, Bytes: best.bytes,
		TargetKbps: seg.TargetKbps, ActualKbps: actual, DevPct: dev, PSNRDB: stats.MeanDB,
		PSNRMinDB: stats.MinDB, PSNRMaxDB: stats.MaxDB, Q: best.avgQ, Capped: capped,
		Status: state.Done}, nil
}

type segmentAttempt struct {
	bytes int64
	recon []source.Frame
	avgQ  float64
	maxQ  float64
}

func encodeAtQ(ctx context.Context, frames []source.Frame, enc *codec.Encoder, baseQ, targetFrameBits float64) (segmentAttempt, error) {
	vbv := ratecontrol.NewVBV(targetFrameBits)
	attempt := segmentAttempt{recon: make([]source.Frame, 0, len(frames))}
	var qsum float64
	var reference *source.Frame
	for i, frame := range frames {
		if err := ctx.Err(); err != nil {
			return segmentAttempt{}, err
		}
		q := clamp(baseQ*vbv.QMultiplier(), 0.5, 255)
		res, err := enc.EncodeFrame(frame, reference, q, i == 0)
		if err != nil {
			return segmentAttempt{}, err
		}
		attempt.bytes += int64(res.Bytes)
		attempt.recon = append(attempt.recon, res.Reconstructed)
		qsum += q
		if q > attempt.maxQ {
			attempt.maxQ = q
		}
		vbv.Observe(float64(res.Bytes) * 8)
		recon := res.Reconstructed
		reference = &recon
	}
	if len(frames) > 0 {
		attempt.avgQ = qsum / float64(len(frames))
	}
	return attempt, nil
}

func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
