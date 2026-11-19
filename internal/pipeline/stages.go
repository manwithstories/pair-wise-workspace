package pipeline

import (
	"context"

	"mediapipeline/internal/clock"
	"mediapipeline/internal/mediaerr"
	"mediapipeline/internal/reorder"
	"mediapipeline/internal/stream"
	"mediapipeline/internal/testdata"
)

// stageProduce demultiplexes datagrams, validates them and tracks per-track
// sequence state. Control-plane failures (bad RTCP) and media-plane failures
// (truncated RTP) are isolated counters, not run aborts.
func (r *Runner) stageProduce(
	ctx context.Context,
	fx *testdata.Fixture,
	out *boundedQueue,
	res *Result,
	addFailure func(*mediaerr.Error),
	fatal FatalFunc,
) {
	for _, d := range fx.Datagrams {
		if d.IsRTCP {
			if merr := stream.ParseRTCP(d.Raw); merr != nil {
				res.add(func(c *Counters) { c.RTCPInvalid++ })
				addFailure(merr)
				continue
			}
			res.add(func(c *Counters) { c.RTCPValid++ })
			continue
		}
		p, merr := stream.ParseRTP(d.Raw, d.Offset, d.Arrival)
		if merr != nil {
			res.add(func(c *Counters) { c.ParseErrors++ })
			addFailure(merr)
			continue
		}
		tr := r.trackerFor(p.SSRC, p.PayloadType)
		if _, terr := tr.Observe(p); terr != nil {
			// A duplicate with conflicting payload is isolated downstream;
			// record it here but still forward for reorder-level handling.
			addFailure(terr)
		}
		r.anchorTimeline(p)
		if merr := r.send(out, ctx, p); merr != nil {
			fatal(merr)
			return
		}
		res.add(func(c *Counters) { c.Parsed++ })
	}
}

// trackerFor lazily creates one sequence tracker per SSRC.
func (r *Runner) trackerFor(ssrc uint32, payloadType uint8) *stream.Tracker {
	r.stateMu.RLock()
	tr, ok := r.trackers[ssrc]
	r.stateMu.RUnlock()
	if ok {
		return tr
	}
	r.stateMu.Lock()
	defer r.stateMu.Unlock()
	if tr, ok = r.trackers[ssrc]; ok { // lost the race, reuse the winner
		return tr
	}
	tr = stream.NewTracker(ssrc)
	r.trackers[ssrc] = tr
	return tr
}

// anchorTimeline fixes each track's nanosecond origin on its first valid packet.
func (r *Runner) anchorTimeline(p *stream.Packet) {
	r.stateMu.RLock()
	tl, ok := r.tlines[p.SSRC]
	r.stateMu.RUnlock()
	if !ok {
		rate := uint32(48000)
		if p.PayloadType == 97 { // video track from the fixture
			rate = 90000
		}
		created, terr := clock.NewTimeline(rate, 0)
		if terr != nil || created == nil {
			return
		}
		r.stateMu.Lock()
		if existing, exists := r.tlines[p.SSRC]; exists {
			tl = existing
		} else {
			tl = created
			r.tlines[p.SSRC] = tl
		}
		r.stateMu.Unlock()
	}
	tl.Anchor(p.Timestamp, 0)
}

// stageReorder pushes every parsed packet into the bounded per-track buffer
// and forwards contiguous in-order packets. Late, conflicting and over-limit
// packets are counted and isolated without aborting the stream.
func (r *Runner) stageReorder(
	ctx context.Context,
	in, out *boundedQueue,
	res *Result,
	addFailure func(*mediaerr.Error),
	fatal FatalFunc,
) {
	for {
		p, ok, merr := in.Receive(ctx)
		if merr != nil {
			fatal(merr)
			return
		}
		if !ok {
			return
		}
		buf := r.reorderFor(p.SSRC)
		gaps, perr := buf.Push(p)
		for _, g := range gaps {
			n := int64(g.To - g.From + 1)
			res.add(func(c *Counters) { c.Gaps += n })
			addFailure(mediaerr.New(mediaerr.StageReorder, mediaerr.KindSequenceGap,
				"reorder window declared a sequence gap").
				WithTrack(p.SSRC))
		}
		if perr != nil {
			switch perr.Kind {
			case mediaerr.KindLate:
				res.add(func(c *Counters) { c.Late++ })
			case mediaerr.KindDuplicate, mediaerr.KindBufferLimit:
				res.add(func(c *Counters) { c.Isolated++ })
			}
			addFailure(perr)
		}
		for _, ready := range buf.Drain() {
			if merr := r.send(out, ctx, ready); merr != nil {
				fatal(merr)
				return
			}
		}
	}
}

// reorderFor lazily creates one bounded reorder buffer per SSRC.
func (r *Runner) reorderFor(ssrc uint32) *reorder.Buffer {
	r.stateMu.RLock()
	b, ok := r.reorders[ssrc]
	r.stateMu.RUnlock()
	if ok {
		return b
	}
	r.stateMu.Lock()
	defer r.stateMu.Unlock()
	if b, ok = r.reorders[ssrc]; ok {
		return b
	}
	var merr *mediaerr.Error
	b, merr = reorder.New(ssrc, reorder.Config{
		WindowSize: r.cfg.ReorderWindow,
		MaxBytes:   int(r.cfg.MaxBufferBytes),
	})
	if merr != nil {
		panic(merr) // validated config cannot fail here
	}
	r.reorders[ssrc] = b
	return b
}
