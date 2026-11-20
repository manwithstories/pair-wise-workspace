package pipeline

import (
	"context"
	"path/filepath"

	"archive/internal/batch"
	"archive/internal/index"
	"archive/internal/model"
	"archive/internal/rtp"
	"archive/internal/testdata"
	"archive/internal/timeline"
)

type statsAccum struct {
	cfg         Config
	stream      *testdata.Stream
	specs       []testdata.TrackSpec
	resume      index.Resume
	inputDigest string
	paramDigest string
	out         Stats
}

func indexDir(cfg Config) string   { return filepath.Join(cfg.OutDir, "index") }
func segmentDir(cfg Config) string { return filepath.Join(cfg.OutDir, "segments") }

// run wires the stages and drives them to completion or cancellation.
func (s *statsAccum) run(ctx context.Context) error {
	cfg := s.cfg
	repair := rtp.NewRepairer(cfg.ReorderWindow)
	mixer := timeline.NewMixer(cfg.DriftLimitNS)
	for i, sp := range s.specs {
		kind := model.TrackVideo
		if sp.Kind == "audio" {
			kind = model.TrackAudio
		}
		mixer.RegisterTrack(sp.SSRC, kind, sp.ClockRate)
		s.specs[i] = sp
	}

	rawCh := make(chan []byte, cfg.Queue)
	itemCh := make(chan streamItem, cfg.Queue)
	frameCh := make(chan model.Frame, cfg.Queue)

	stageErr := make(chan error, 4)

	// Producer: feed the deterministic capture with backpressure.
	go func() {
		stageErr <- s.produce(ctx, rawCh)
	}()
	// Parse stage: datagrams -> packets or RTCP reports. It never touches the
	// mixer; reports are forwarded as items so all mixer access stays on the
	// single repair goroutine (no shared-state data race).
	go func() {
		stageErr <- s.parse(ctx, rawCh, itemCh)
	}()
	// Repair stage: bounded reorder/dedup/loss, applies SRs and emits frames.
	go func() {
		stageErr <- s.repairStage(ctx, repair, itemCh, frameCh, mixer)
	}()
	// Consumer: render + index, optionally slow to exercise backpressure.
	consumeErr := s.consume(ctx, frameCh)

	// Collect the three producer-side stage results.
	var firstErr error
	for i := 0; i < 3; i++ {
		if err := <-stageErr; err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if err := consumeErr; err != nil && firstErr == nil {
		firstErr = err
	}

	// Repair stats and timeline diagnostics.
	rs := repair.Stats()
	s.out.Emitted = rs.Emitted
	s.out.Lost = rs.Lost
	s.out.Duplicates = rs.Duplicates
	s.out.Reordered = rs.Reordered
	s.out.PeakBuffer = rs.PeakBuffer
	s.out.LostRanges = rs.LostRanges
	s.out.DriftEvents = mixer.Drift()
	s.out.MissingReports = mixer.MissingReports()
	s.out.RecoveredChunks = s.resume.RecoveredChunks

	// Preserve committed checkpoints on cancellation/write failure.
	if firstErr != nil {
		if me := model.AsError(firstErr); me != nil && me.Kind == model.KindCanceled {
			s.out.Canceled = true
		}
		st := batch.State{
			InputDigest: s.out.InputDigest, ParamDigest: s.out.ParamDigest,
			Segments: s.out.Segments, Frames: s.out.Frames, Canceled: s.out.Canceled,
		}
		_ = batch.MarkCanceled(cfg.OutDir, st)
	}
	return firstErr
}

func (s *statsAccum) produce(ctx context.Context, out chan<- []byte) error {
	defer close(out)
	for _, d := range s.stream.Datagrams {
		select {
		case <-ctx.Done():
			return model.New(model.KindCanceled, "producer", "canceled while feeding input", ctx.Err())
		case out <- d:
		}
	}
	return nil
}

// streamItem is the unit handed from the parser to the repair goroutine:
// either an RTP packet or an RTCP Sender Report. Routing both through one
// channel guarantees the mixer is only ever touched by one goroutine.
type streamItem struct {
	packet model.Packet
	report model.SenderReport
	isSR   bool
}

func (s *statsAccum) parse(ctx context.Context, in <-chan []byte, out chan<- streamItem) error {
	defer close(out)
	for raw := range in {
		dg, err := rtp.ParseDatagram(raw)
		if err != nil {
			// Truncated/malformed datagram: count and skip, never fabricate.
			s.out.Truncated++
			continue
		}
		s.out.Parsed++
		item := streamItem{}
		if dg.Kind == rtp.KindRTCP {
			item = streamItem{report: dg.Report, isSR: true}
		} else {
			item = streamItem{packet: s.annotate(dg.Packet)}
		}
		select {
		case <-ctx.Done():
			return model.New(model.KindCanceled, "parser", "canceled", ctx.Err())
		case out <- item:
		}
	}
	return nil
}

// annotate attaches track metadata and derives the keyframe marker from the
// RTP marker bit set by the fixture generator.
func (s *statsAccum) annotate(p model.Packet) model.Packet {
	for _, sp := range s.specs {
		if sp.SSRC == p.SSRC {
			if sp.Kind == "audio" {
				p.Track = model.TrackAudio
			} else {
				p.Track = model.TrackVideo
			}
			p.ClockRate = sp.ClockRate
			if sp.KeyEvery > 1 && p.Sequence%uint16(sp.KeyEvery) == 0 {
				p.Keyframe = true
			}
			break
		}
	}
	return p
}

func (s *statsAccum) repairStage(ctx context.Context, repair *rtp.Repairer,
	in <-chan streamItem, out chan<- model.Frame, mixer *timeline.Mixer) error {
	defer close(out)
	repair.Emit = func(p model.Packet) {
		frame, err := mixer.Mix(p)
		if err != nil {
			return // clock failures are surfaced via mixer diagnostics
		}
		frame.PayloadHash = payloadHash(p)
		frame.SourceStart, frame.SourceEnd = p.ExtSequence, p.ExtSequence
		frame.SSRC = p.SSRC
		frame.Keyframe = p.Keyframe
		frame.Track = p.Track
		select {
		case <-ctx.Done():
			return
		case out <- frame:
		}
	}
	for {
		select {
		case <-ctx.Done():
			return model.New(model.KindCanceled, "repair", "canceled", ctx.Err())
		case item, ok := <-in:
			if !ok {
				repair.Flush()
				return nil
			}
			if item.isSR {
				mixer.ApplySR(item.report)
				continue
			}
			repair.Feed(item.packet)
		}
	}
}
