// Package pipeline wires ingest, timeline, DSP, recovery and manifest output.
package pipeline

import (
	"bytes"
	"errors"
	"fmt"
	"io"

	"github.com/pion/rtp"

	"github.com/clearliu/mediq/internal/core"
	"github.com/clearliu/mediq/internal/ingest"
	"github.com/clearliu/mediq/internal/source"
	"github.com/clearliu/mediq/internal/timeline"
)

// Analysis is the deterministic result of reading and ordering one feed.
type Analysis struct {
	Clock        *core.Clock
	Reorder      *ingest.ReorderBuffer
	Watermark    *timeline.Watermark
	Packets      map[int64]*rtp.Packet
	RejectedLate map[int64]bool

	ParseErrors      int
	FirstOffset      int64
	FirstError       string
	RTCPFrames       int
	OutOfOrder       int
	DuplicatePackets int

	ExtFirst int64
	ExtLast  int64
}

// OutputFramesPerPacket maps one source packet duration to 48 kHz frames.
func OutputFramesPerPacket(spec source.Spec) int64 {
	return int64(spec.FramesPerPacket) * core.OutputRateHz / int64(spec.SampleRate)
}

// Analyze parses and orders an in-memory local fixture stream.
func Analyze(input []byte, spec source.Spec) (*Analysis, error) {
	outFrames := OutputFramesPerPacket(spec)
	a := &Analysis{
		Clock:        core.NewClock(spec.SSRC, uint32(spec.SampleRate)),
		Reorder:      ingest.NewReorderBuffer(spec.ReorderWindow),
		Watermark:    timeline.New(timeline.Config{IntervalSamples: int64(spec.IntervalSamples), RetentionIntervals: int64(spec.RetentionIntervals)}),
		Packets:      make(map[int64]*rtp.Packet),
		RejectedLate: make(map[int64]bool),
		FirstOffset:  -1,
	}
	r := ingest.NewReader(bytes.NewReader(input), ingest.DefaultMaxDatagramBytes)
	haveFirst := false

	for {
		rec, err := r.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			var ce *core.Error
			if errors.As(err, &ce) && ce.Kind == core.KindParse {
				a.ParseErrors++
				if a.FirstOffset < 0 {
					a.FirstOffset = ce.Offset
					a.FirstError = ce.Error()
				}
				// A framed validation error still has a consumed body.
				if rec == nil {
					return nil, err
				}
				continue
			}
			return nil, err
		}

		switch rec.Kind {
		case ingest.KindRTCP:
			a.RTCPFrames++
			if rec.SR != nil {
				a.Clock.ObserveSenderReport(rec.SR.RTPTimestamp, rec.SR.NTPSeconds, rec.SR.NTPFraction)
			}
			continue
		case ingest.KindRTP:
			// Feed below.
		default:
			a.ParseErrors++
			continue
		}

		res := a.Reorder.Feed(rec)
		if !haveFirst && len(res.Ordered) > 0 {
			a.ExtFirst = res.Ordered[0].ExtSeq
			a.ExtLast = a.ExtFirst + int64(spec.PacketCount) - 1
			haveFirst = true
		}
		for _, g := range res.Gaps {
			a.observeSpan(g.ExtSeq, outFrames)
		}
		for _, op := range res.Ordered {
			a.Packets[op.ExtSeq] = op.Record.Packet
			a.observeSpan(op.ExtSeq, outFrames)
		}
		for _, op := range res.Late {
			a.Packets[op.ExtSeq] = op.Record.Packet
			_, _, rejected := a.observeSpan(op.ExtSeq, outFrames)
			if rejected {
				delete(a.Packets, op.ExtSeq)
				a.RejectedLate[op.ExtSeq] = true
			}
		}
	}

	flush := a.Reorder.Flush()
	for _, g := range flush.Gaps {
		a.observeSpan(g.ExtSeq, outFrames)
	}
	for _, op := range flush.Ordered {
		a.Packets[op.ExtSeq] = op.Record.Packet
		a.observeSpan(op.ExtSeq, outFrames)
	}
	a.Watermark.Flush()
	a.DuplicatePackets = a.Reorder.Stats.DuplicatePackets
	a.OutOfOrder = a.Reorder.Stats.OutOfOrderPackets
	return a, nil
}

func (a *Analysis) observeSpan(extSeq, outFrames int64) ([]timeline.Range, *timeline.Checkpoint, bool) {
	ev := timeline.Event{
		Sequence: extSeq,
		Start:    core.Time(extSeq * outFrames),
		End:      core.Time((extSeq + 1) * outFrames),
	}
	return a.Watermark.Observe(ev)
}

// MissingPackets returns extended sequence slots with no accepted packet.
func (a *Analysis) MissingPackets() []int64 {
	var missing []int64
	for ext := a.ExtFirst; ext <= a.ExtLast; ext++ {
		if _, ok := a.Packets[ext]; !ok {
			missing = append(missing, ext)
		}
	}
	return missing
}

// MissingSamples returns missing 48 kHz output frames.
func (a *Analysis) MissingSamples(spec source.Spec) int64 {
	return int64(len(a.MissingPackets())) * OutputFramesPerPacket(spec)
}

// FormatSeqRanges compresses sorted extended sequence numbers into ranges.
func FormatSeqRanges(seqs []int64) []string {
	var ranges []string
	for i := 0; i < len(seqs); {
		j := i
		for j+1 < len(seqs) && seqs[j+1] == seqs[j]+1 {
			j++
		}
		if i == j {
			ranges = append(ranges, fmt.Sprintf("%d", seqs[i]))
		} else {
			ranges = append(ranges, fmt.Sprintf("%d-%d", seqs[i], seqs[j]))
		}
		i = j + 1
	}
	return ranges
}
