package pipeline

import (
	"context"
	"os"
	"path/filepath"
	"time"

	"archive/internal/batch"
	"archive/internal/index"
	"archive/internal/model"
	"archive/internal/render"
	"archive/internal/testdata"
)

// framesPerSegment bounds one segment so render memory stays fixed. With
// 12000 packets and 2 tracks interleaved, the runner targets ~12 segments.
const framesPerSegment = 1000

// consume reads frames, renders them into fixed-size segments and indexes
// keyframes into fixed-packet chunks. A per-frame artificial delay models a
// slow disk; because frameCh is bounded, the producer blocks rather than
// buffering the whole capture.
func (s *statsAccum) consume(ctx context.Context, in <-chan model.Frame) error {
	cfg := s.cfg
	if err := os.MkdirAll(segmentDir(cfg), 0o755); err != nil {
		return model.New(model.KindIO, cfg.OutDir, "cannot create segment dir", err)
	}
	store, err := index.NewStore(indexDir(cfg), cfg.ChunkPackets)
	if err != nil {
		return err
	}

	var seg *render.SegmentWriter
	var ordinal int64
	segIndex := 0

	flush := func() error {
		if seg == nil || seg.Len() == 0 {
			return nil
		}
		done, err := seg.Flush()
		if err != nil {
			return err
		}
		s.out.Segments++
		s.out.SegmentSHA = append(s.out.SegmentSHA, done.SHA256)
		seg = nil
		return nil
	}

	for {
		var frame model.Frame
		select {
		case <-ctx.Done():
			_ = flush() // keep what was committed
			_ = store.FlushTail()
			_ = store.SaveManifest(s.inputDigest, s.paramDigest)
			return model.New(model.KindCanceled, "consumer", "canceled; checkpoints preserved", ctx.Err())
		case f, ok := <-in:
			if !ok {
				if err := flush(); err != nil {
					return err
				}
				if err := store.FlushTail(); err != nil {
					return err
				}
				return store.SaveManifest(s.inputDigest, s.paramDigest)
			}
			frame = f
		}

		if seg == nil {
			seg = render.NewSegmentWriter(segmentDir(cfg), segIndex, ordinal, frame.TimeNanos)
		}
		seg.Add(render.FrameRecord{
			Index: s.out.Frames, Track: frame.Track, TimeNanos: frame.TimeNanos,
			SourceStart: frame.SourceStart, SourceEnd: frame.SourceEnd, SSRC: frame.SSRC,
			Keyframe: frame.Keyframe, PayloadHash: frame.PayloadHash, ClockBasis: frame.ClockBasis,
		})
		s.out.Frames++
		if frame.Keyframe {
			s.out.Keyframes++
			if err := store.Add(index.KeyframeEntry{
				SSRC: frame.SSRC, Sequence: frame.SourceStart, TimeNanos: frame.TimeNanos,
				FrameIndex: s.out.Frames - 1, Segment: segIndex,
			}, ordinal); err != nil {
				return err
			}
			s.out.KeyframeIndexed++
		}
		ordinal++

		if seg.Len() >= framesPerSegment {
			if err := flush(); err != nil {
				return err
			}
			segIndex++
		}
		if cfg.SlowWriterMs > 0 {
			sleep(ctx, time.Duration(cfg.SlowWriterMs)*time.Millisecond)
		}
	}
}

// sleep honors cancellation while emulating slow-disk latency.
func sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// score computes the acceptance rates against generator ground truth and
// records the checkpoint outcome.
func (s *statsAccum) score(truth testdata.GroundTruth) {
	// Round-trip: fraction of the packets that survived transport and
	// parsed cleanly which the repairer emitted in order.
	recoverable := truth.TotalPackets - truth.Lost - truth.Truncated
	if recoverable <= 0 {
		s.out.RoundtripRate = 1
	} else {
		s.out.RoundtripRate = float64(s.out.Emitted) / float64(recoverable)
	}
	if truth.Keyframes > 0 {
		s.out.KeyframeHitRate = float64(s.out.KeyframeIndexed) / float64(truth.Keyframes)
	} else {
		s.out.KeyframeHitRate = 1
	}
	s.out.Received = truth.TotalPackets
}

func (s *statsAccum) snapshot() *Stats {
	cp := s.out
	return &cp
}

// buildTrackSpecs creates the requested number of deterministic tracks,
// alternating video and audio, splitting the packet budget evenly.
func buildTrackSpecs(cfg Config) []testdata.TrackSpec {
	per := cfg.Packets / cfg.Tracks
	if per < 1 {
		per = 1
	}
	specs := make([]testdata.TrackSpec, 0, cfg.Tracks)
	for i := 0; i < cfg.Tracks; i++ {
		if i%2 == 0 {
			specs = append(specs, testdata.TrackSpec{
				SSRC: 0xA110CE00 + uint32(i), ClockRate: 90000, PayloadPT: 96,
				Packets: per, KeyEvery: 25, TSInterval: 900, Kind: "video",
			})
		} else {
			specs = append(specs, testdata.TrackSpec{
				SSRC: 0xA110CE00 + uint32(i), ClockRate: 48000, PayloadPT: 97,
				Packets: per, KeyEvery: 0, TSInterval: 480, Kind: "audio",
			})
		}
	}
	return specs
}

var _ = filepath.Join
var _ = batch.Params{}
