// Package pipeline wires the reusable media kernel stages together.
package pipeline

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/pairwise/mediapack/internal/audio"
	"github.com/pairwise/mediapack/internal/cut"
	"github.com/pairwise/mediapack/internal/demux"
	"github.com/pairwise/mediapack/internal/timeline"
	"github.com/pairwise/mediapack/internal/transport"
)

// Options configures bounded streaming and audio analysis.
type Options struct {
	BufferPackets int
	ConsumerDelay time.Duration
	FrameDuration time.Duration
	Silence       audio.Config
}

// Report contains cross-module results used by verification.
type Report struct {
	Packets        int
	Gaps           []transport.CCGap
	PCRWraps       int
	PTSWraps       int
	BadCRC         int
	PSIVersion     int
	Frames         []demux.Frame
	Cut            *cut.Result
	Truncated      *transport.TruncatedError
	QueueHighWater int
	ClockGaps      []timeline.Interval
}

// Run processes a length-bounded TS stream through parsing, clock extension,
// PES assembly, cutting, and silence detection.
func Run(ctx context.Context, r io.Reader, opts Options) (*Report, error) {
	if opts.BufferPackets <= 0 {
		opts.BufferPackets = 64
	}
	if opts.FrameDuration <= 0 {
		opts.FrameDuration = 40 * time.Millisecond
	}
	tr := transport.NewReader(r)
	q := transport.NewQueue(opts.BufferPackets, opts.ConsumerDelay)
	rep := &Report{}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	wg.Add(1)
	var readErr error
	go func() {
		defer wg.Done()
		defer q.Close()
		for {
			p, err := tr.Read()
			if errors.Is(err, io.EOF) {
				return
			}
			if err != nil {
				readErr = err
				return
			}
			if err := q.Push(runCtx, p); err != nil {
				readErr = err
				return
			}
		}
	}()

	reg := transport.NewPSIRegistry()
	clock := &timeline.Clock{}
	dmx := demux.NewDemuxer()
	ccTracker := transport.NewCCTracker()
	prevVideoPTS := time.Duration(0)
	lastPCRTime := time.Duration(0)

	consumeErr := func() error {
		for {
			p, err := q.Pop(runCtx)
			if errors.Is(err, io.EOF) {
				return nil
			}
			if err != nil {
				return err
			}
			rep.Packets++

			if p.HasPCR {
				pcrTime, err := clock.FeedPCRRelative(p.PCR)
				if err != nil {
					return err
				}
				lastPCRTime = pcrTime
			}

			if p.PID == 0 || reg.KnownPMTPIDs[p.PID] {
				t, err := reg.HandlePacket(p)
				switch {
				case errors.Is(err, transport.ErrPSIWaitingConfirm):
					// First consistent copy is pending; retain previous version.
				case errors.Is(err, transport.ErrBadCRC):
					// Registry counted it and retained last-known-good PSI.
				case err != nil:
					return err
				case t != nil:
					if err := applyPSI(t, reg, dmx, rep); err != nil {
						return err
					}
				}
			}

			frame, err := dmx.Push(p)
			if err != nil {
				return err
			}
			if frame != nil {
				// All elementary tracks share the program PTS origin/extension.
				pts, ticks, err := clock.FeedPTSRelative(frame.PTSTicks)
				if err != nil {
					return err
				}
				frame.PTS = pts
				frame.PTSTicks = ticks
				rep.Frames = append(rep.Frames, *frame)
			}

			if g := ccTracker.Check(p); g != nil {
				start := prevVideoPTS
				end := lastPCRTime
				if frame != nil && frame.Codec == "h264-synthetic" {
					end = frame.PTS
				}
				if end < start {
					end = start + opts.FrameDuration
				}
				clock.MarkGap(start, end,
					fmt.Sprintf("cc-gap pid=0x%x missing=%d", g.PID, g.Missing))
			}
			if frame != nil && frame.Codec == "h264-synthetic" {
				prevVideoPTS = frame.PTS
			}
		}
	}()

	cancel()
	wg.Wait()
	rep.QueueHighWater = q.HighWater()
	rep.Gaps = ccTracker.Gaps()
	rep.PCRWraps = clock.WrapCount()
	rep.PTSWraps = clock.PTSWrapCount
	rep.BadCRC = reg.BadCRC
	rep.PSIVersion = reg.PMTVersion(0x1000)
	rep.ClockGaps = append([]timeline.Interval(nil), clock.Gaps...)

	var te *transport.TruncatedError
	if errors.As(readErr, &te) {
		rep.Truncated = te
	} else if readErr != nil && consumeErr == nil && !errors.Is(readErr, context.Canceled) {
		return rep, readErr
	}
	if consumeErr != nil {
		return rep, consumeErr
	}

	cutRes, err := cut.Build(rep.Frames, clock, cut.Config{FrameDuration: opts.FrameDuration})
	if err != nil {
		return rep, err
	}
	for _, seg := range cutRes.Segments {
		if len(seg.AudioPCM) == 0 {
			continue
		}
		intervals, err := audio.Analyze(seg.Start, seg.AudioPCM, opts.Silence)
		if err != nil {
			return rep, err
		}
		seg.Silence = intervals
	}
	rep.Cut = cutRes
	return rep, nil
}

func applyPSI(t *transport.PSITable, reg *transport.PSIRegistry, dmx *demux.Demuxer, rep *Report) error {
	if t.TableID != 0x02 {
		return nil
	}
	_, streams, err := transport.ParsePMTEntries(t)
	if err != nil {
		return err
	}
	for _, s := range streams {
		if err := dmx.SetTrack(s.PID, s.Codec, int(t.Version)); err != nil {
			return err
		}
	}
	if v := reg.PMTVersion(0x1000); v >= 0 {
		rep.PSIVersion = v
	}
	return nil
}
