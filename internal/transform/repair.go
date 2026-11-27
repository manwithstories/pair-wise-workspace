// Package transform turns a scanned segment into a repaired record stream.
//
// Records that overlap a damaged byte range — a CRC failure, a truncated
// record or a clip boundary that fell inside a record — are isolated and left
// out of the output. Every excluded span keeps its original source offset and
// is annotated with the timeline clips it belongs to, so a parse error can be
// followed from parsing through transformation into verification.
package transform

import (
	"context"
	"sort"

	"relayaudit/internal/fault"
	"relayaudit/internal/timeline"
	"relayaudit/internal/wire"
)

// Range is an isolated source span. Start and End are original byte offsets.
type Range struct {
	Start, End int64
	Kind       fault.Kind
	Track      uint8
	// Ticks is the shared-timebase position where the span begins.
	Ticks uint64
	// Clips lists the editorial labels overlapping the span.
	Clips []string
	// Missing is the number of declared bytes that fell past end of input
	// for a truncated span; zero for other kinds.
	Missing int64
}

// Length returns the span size in bytes.
func (r Range) Length() int64 { return r.End - r.Start }

// BuildRanges converts parse and timeline faults into merged isolation spans.
// Index faults are excluded: a bad index is rebuilt, not isolated. Overlapping
// spans are merged so each damaged region is counted once.
func BuildRanges(faults []*fault.Fault, sourceSize int64) []Range {
	var out []Range
	for _, f := range faults {
		if f == nil || f.Kind() == fault.Index || f.Kind() == fault.Checkpoint || f.Kind() == fault.Canceled {
			continue
		}
		end := f.Offset() + f.Length()
		missing := int64(0)
		switch {
		case f.Kind() == fault.Truncated:
			// Truncation always bites at the end of the input. The fault's
			// length is how many bytes the declared record still expected;
			// that is exactly how much of the tail is missing.
			missing = f.Length()
			end = sourceSize
		case f.Length() <= 0:
			end = f.Offset() + 1
		}
		if end > sourceSize {
			end = sourceSize
		}
		if f.Offset() >= sourceSize {
			continue
		}
		out = append(out, Range{Start: f.Offset(), End: end, Kind: f.Kind(), Track: f.Track(), Missing: missing})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Start != out[j].Start {
			return out[i].Start < out[j].Start
		}
		return out[i].End < out[j].End
	})
	merged := out[:0]
	for _, r := range out {
		if n := len(merged); n > 0 && r.Start < merged[n-1].End {
			if r.End > merged[n-1].End {
				merged[n-1].End = r.End
			}
			if r.Missing > merged[n-1].Missing {
				merged[n-1].Missing = r.Missing
			}
			continue
		}
		merged = append(merged, r)
	}
	return merged
}

// Annotate attaches timeline ticks and clip labels to each range.
func Annotate(ranges []Range, res *timeline.Resolution) {
	if res == nil {
		return
	}
	for i := range ranges {
		r := &ranges[i]
		if r.Track != 0 {
			if tick, ok := res.TickNearOffset(r.Track, r.Start); ok {
				r.Ticks = tick
				r.Clips = res.SpansOverlapping(r.Track, tick, tick+1)
			}
		}
		if r.Clips == nil {
			r.Clips = []string{}
		}
	}
}

// Overlaps reports whether a record intersects any isolated range.
func Overlaps(rec *wire.Record, ranges []Range) (Range, bool) {
	start, end := rec.Offset, rec.End()
	for _, r := range ranges {
		if r.Start < end && start < r.End {
			return r, true
		}
	}
	return Range{}, false
}

// Stats summarises a repair pass.
type Stats struct {
	Kept    int
	Dropped int
	Ranges  []Range
}

// Repair performs a single-pass repair without checkpointing. It exists for
// library callers that already own their output; the pipeline uses Encoder
// directly so it can commit checkpoints between batches.
func Repair(ctx context.Context, scan *wire.ScanResult, res *timeline.Resolution, faults []*fault.Fault) (*Stats, []byte, error) {
	ranges := BuildRanges(append(append([]*fault.Fault{}, scan.Damage...), faults...), scan.SourceSize)
	Annotate(ranges, res)
	e := NewEncoder()
	for i, rec := range scan.Records {
		if err := ctx.Err(); err != nil {
			return nil, nil, fault.Wrap(fault.Canceled, rec.Offset, err, "repair canceled after %d records", i)
		}
		if _, hit := Overlaps(rec, ranges); hit {
			e.Drop(rec.Offset)
			continue
		}
		_ = i
		e.Add(rec)
	}
	out, err := e.Finalize(uint8(scan.Header.Tracks))
	if err != nil {
		return nil, nil, err
	}
	// Records declared by the header but absent from the repaired output are
	// the damaged ones: they either failed to parse or overlapped an
	// isolation range. Clamping keeps a stale declared count from going
	// negative.
	dropped := int(scan.Header.RecordCount) - e.Kept()
	if dropped < 0 {
		dropped = 0
	}
	return &Stats{Kept: e.Kept(), Dropped: dropped, Ranges: ranges}, out, nil
}
