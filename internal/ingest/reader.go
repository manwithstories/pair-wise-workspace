package ingest

import (
	"context"
	"fmt"
	"sort"
	"time"

	"mediacheck/internal/model"
)

// Reader replays a generated fixture as a packet event stream.  It enforces
// the hard bounds of the package, tracks sequence numbers per stream and
// emits faults for duplicates, losses, wraps and timestamp regressions.
// Reading pauses whenever the consumer is slow because Emit is allowed to
// block; the pipeline's bounded queue is what actually applies backpressure.
type Reader struct {
	fixture  *model.Fixture
	trackers map[model.Track]*SequenceTracker
	order    []int
	next     int
	emitted  int
	bytes    int64
	limits   ReaderLimits
	stats    Stats
	lastPTS  map[model.Track]int64
	prevPkt  map[model.Track]model.Packet
}

// ReaderLimits are the hard caps applied by a Reader.
type ReaderLimits struct {
	MaxPackets int
	MaxBytes   int64
	MaxSpan    time.Duration
}

// DefaultReaderLimits returns the limits used by the demo command.
func DefaultReaderLimits() ReaderLimits {
	return ReaderLimits{
		MaxPackets: MaxPackets,
		MaxBytes:   512 << 20,
		MaxSpan:    MaxDuration,
	}
}

// NewReader builds a reader over fixture.  Packets are emitted in ascending
// slot order, alternating tracks so both are exercised concurrently.
func NewReader(fixture *model.Fixture, limits ReaderLimits) (*Reader, error) {
	if fixture == nil || len(fixture.Packets) == 0 {
		return nil, fmt.Errorf("ingest: empty fixture")
	}
	if limits.MaxPackets <= 0 {
		limits.MaxPackets = MaxPackets
	}
	if limits.MaxBytes <= 0 {
		limits.MaxBytes = 512 << 20
	}
	if limits.MaxSpan <= 0 {
		limits.MaxSpan = MaxDuration
	}
	order := make([]int, len(fixture.Packets))
	for i := range order {
		order[i] = i
	}
	// Stable sort by slot start keeps equal timestamps in generation order.
	stableSortBySlot(order, fixture.Packets)
	r := &Reader{
		fixture:  fixture,
		trackers: map[model.Track]*SequenceTracker{},
		order:    order,
		limits:   limits,
		lastPTS:  map[model.Track]int64{},
		prevPkt:  map[model.Track]model.Packet{},
	}
	for _, s := range fixture.Streams {
		r.trackers[s.Track] = NewSequenceTracker(s.Track)
	}
	return r, nil
}

// Event is one observation handed to the pipeline.  Exactly one Packet is
// set for carried events; a dropped duplicate or lost slot carries only a
// Fault.
type Event struct {
	Packet *model.Packet
	Fault  *model.Fault
}

// Stats summarises what the reader saw.
type Stats struct {
	Emitted     int
	Bytes       int64
	Duplicates  int
	Lost        int
	Wraps       int
	Regressions int
	Faults      int
}

// Stats returns a snapshot of reader counters.
func (r *Reader) Stats() Stats { return r.stats }

// EmitFunc receives each event.  Returning an error stops the reader and is
// propagated unchanged, which is how cancellation and queue closure reach
// the read stage.
type EmitFunc func(Event) error

// Run replays the fixture until exhausted, ctx is cancelled or emit fails.
func (r *Reader) Run(ctx context.Context, emit EmitFunc) error {
	for r.next < len(r.order) {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("%w: %v", model.ErrCanceled, err)
		}
		i := r.order[r.next]
		r.next++
		src := r.fixture.Packets[i]
		if err := r.accept(src, emit); err != nil {
			return err
		}
	}
	return nil
}

func (r *Reader) accept(src model.Packet, emit EmitFunc) error {
	if r.emitted >= r.limits.MaxPackets {
		return fmt.Errorf("ingest: packet limit %d: %w", r.limits.MaxPackets, model.ErrLimitExceeded)
	}
	if r.bytes+int64(len(src.Data)) > r.limits.MaxBytes {
		return fmt.Errorf("ingest: byte limit %d: %w", r.limits.MaxBytes, model.ErrLimitExceeded)
	}
	if src.Slot.Start > int64(r.limits.MaxSpan) {
		return fmt.Errorf("ingest: span limit %s: %w", r.limits.MaxSpan, model.ErrLimitExceeded)
	}
	if !src.Slot.Valid() {
		return r.fault(src, model.Fault{Kind: model.FaultZeroDuration, Note: "non-positive slot"}, emit)
	}
	if len(src.Data) > MaxPacketBytes {
		return fmt.Errorf("ingest: packet %d bytes exceeds limit %d: %w", len(src.Data), MaxPacketBytes, model.ErrLimitExceeded)
	}
	tracker := r.trackers[src.Track]
	if tracker == nil {
		return fmt.Errorf("ingest: no tracker for track %v", src.Track)
	}
	res := tracker.Observe(src.Sequence)
	if res.Duplicate {
		r.stats.Duplicates++
		return r.fault(src, model.Fault{
			Kind: model.FaultDuplicate,
			Note: fmt.Sprintf("seq %d repeated, dropped", src.Sequence),
		}, emit)
	}
	if res.Wrapped {
		r.stats.Wraps++
		if err := r.fault(src, model.Fault{Kind: model.FaultSequenceWrap, Note: "counter wrapped"}, emit); err != nil {
			return err
		}
	}
	if res.Lost > 0 {
		r.stats.Lost += res.Lost
		slot := model.Slot{Start: src.Slot.Start, End: src.Slot.Start}
		if prev, ok := r.prevPkt[src.Track]; ok {
			// Anchor the loss to the real hole between the previous accepted
			// packet and this one, so damaged ranges locate it precisely.
			if slots := ExpectedSlots(prev, src, res.Lost); len(slots) > 0 {
				slot = model.Slot{Start: slots[0].Start, End: slots[len(slots)-1].End}
			}
		}
		if err := r.fault(src, model.Fault{
			Kind: model.FaultLoss,
			Note: fmt.Sprintf("%d slot(s) missing before seq %d", res.Lost, src.Sequence),
			Slot: slot,
		}, emit); err != nil {
			return err
		}
	}
	if last, ok := r.lastPTS[src.Track]; ok && src.PTS < last {
		// A pure timestamp regression (sequence numbers intact) is reported
		// with its exact offset and scanning continues.
		r.stats.Regressions++
		if err := r.fault(src, model.Fault{
			Kind: model.FaultTimestampRegres,
			Note: fmt.Sprintf("pts offset %d ticks", src.PTS-last),
		}, emit); err != nil {
			return err
		}
	} else if res.Offset < 0 {
		r.stats.Regressions++
		if err := r.fault(src, model.Fault{
			Kind: model.FaultTimestampRegres,
			Note: fmt.Sprintf("sequence offset %d", res.Offset),
		}, emit); err != nil {
			return err
		}
	}
	// Advance the watermark on every packet so one regressed timestamp does
	// not make all following packets look regressed too.
	if last, ok := r.lastPTS[src.Track]; !ok || src.PTS > last {
		r.lastPTS[src.Track] = src.PTS
	}
	r.prevPkt[src.Track] = src
	pkt := src.Clone()
	if err := emit(Event{Packet: &pkt}); err != nil {
		return err
	}
	r.emitted++
	r.bytes += int64(len(pkt.Data))
	r.stats.Emitted++
	r.stats.Bytes += int64(len(pkt.Data))
	return nil
}

func (r *Reader) fault(src model.Packet, f model.Fault, emit EmitFunc) error {
	f.Track = src.Track
	if f.Seq == 0 {
		f.Seq = uint64(src.Sequence)
	}
	if f.Slot == (model.Slot{}) {
		f.Slot = src.Slot
	}
	r.stats.Faults++
	return emit(Event{Fault: &f})
}

func stableSortBySlot(order []int, packets []model.Packet) {
	// Insertion of a small key function avoids importing sort for a single
	// use and keeps the comparator obvious.
	sort.SliceStable(order, func(a, b int) bool {
		pa, pb := packets[order[a]], packets[order[b]]
		if pa.Slot.Start != pb.Slot.Start {
			return pa.Slot.Start < pb.Slot.Start
		}
		return pa.Sequence < pb.Sequence
	})
}
