// Package timeline orders events by event time under a lateness watermark.
// Arrival order only drives watermark advancement; media order always follows
// the unified event time. Late events within the window trigger a deterministic
// replay of the affected segments, events past the window are quarantined.
package timeline

import (
	"fmt"
	"sort"

	"example.com/medialab/archive/internal/model"
	"example.com/medialab/archive/internal/recovery"
)

// Config freezes the timeline geometry. All values in microseconds.
type Config struct {
	SegmentUS    int64
	LateWindowUS int64
	// MaxPacketUS bounds any single packet; it delays hard finalisation so a
	// late packet cannot overlap an already-final segment.
	MaxPacketUS int64
	// MaxFillUS caps silence fill; longer gaps stay unrecoverable.
	MaxFillUS int64
}

// Validate checks the timeline configuration.
func (c Config) Validate() error {
	if c.SegmentUS <= 0 || c.LateWindowUS < 0 || c.MaxPacketUS <= 0 {
		return fmt.Errorf("timeline: invalid config segment=%d window=%d maxpacket=%d", c.SegmentUS, c.LateWindowUS, c.MaxPacketUS)
	}
	if c.LateWindowUS > c.SegmentUS {
		return fmt.Errorf("timeline: late window %d exceeds segment %d", c.LateWindowUS, c.SegmentUS)
	}
	return nil
}

// Watermark tracks the maximum observed event end and derives the exclusion
// point. It is monotonic.
type Watermark struct {
	lateWindowUS int64
	maxEndUS     int64
	set          bool
}

// NewWatermark returns a watermark with the given lateness window.
func NewWatermark(windowUS int64) *Watermark {
	if windowUS < 0 {
		windowUS = 0
	}
	return &Watermark{lateWindowUS: windowUS}
}

// WindowUS returns the configured lateness window.
func (w *Watermark) WindowUS() int64 { return w.lateWindowUS }

// SetLateWindow installs the window (used before events are observed).
func (w *Watermark) SetLateWindow(windowUS int64) {
	if windowUS >= 0 {
		w.lateWindowUS = windowUS
	}
}

// State exports the watermark for checkpointing.
func (w *Watermark) State() (maxEndUS int64, set bool, windowUS int64) {
	return w.maxEndUS, w.set, w.lateWindowUS
}

// Restore reloads a checkpointed watermark.
func (w *Watermark) Restore(maxEndUS int64, set bool, windowUS int64) {
	w.maxEndUS, w.set, w.lateWindowUS = maxEndUS, set, windowUS
}

// Observe advances the watermark with one event end.
func (w *Watermark) Observe(endUS int64) {
	if !w.set || endUS > w.maxEndUS {
		w.maxEndUS = endUS
		w.set = true
	}
}

// Value returns the current watermark (maxEnd - lateWindow). It is 0 until an
// event has been observed.
func (w *Watermark) Value() int64 {
	if !w.set {
		return 0
	}
	v := w.maxEndUS - w.lateWindowUS
	if v < 0 {
		return 0
	}
	return v
}

// Set reports whether the watermark has been observed.
func (w *Watermark) Set() bool { return w.set }

// MaxEnd returns the highest event end observed.
func (w *Watermark) MaxEnd() int64 { return w.maxEndUS }

// Late reports whether an event is later than the window allows. Zero-length
// events use their start.
func (w *Watermark) Late(startUS, endUS int64) bool {
	if !w.set {
		return false
	}
	t := endUS
	if t < startUS {
		t = startUS
	}
	return t <= w.Value()
}

// SortedEvents returns a deterministic order: event start, then stream, then
// sequence, then payload hash. This order is the canonical basis for segment
// content and output hashes.
func SortedEvents(events []*model.Event) []*model.Event {
	out := append([]*model.Event(nil), events...)
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.ExtTimeUS != b.ExtTimeUS {
			return a.ExtTimeUS < b.ExtTimeUS
		}
		if a.ID.Stream != b.ID.Stream {
			return a.ID.Stream < b.ID.Stream
		}
		if a.ID.Seq != b.ID.Seq {
			return a.ID.Seq < b.ID.Seq
		}
		return a.PayloadSHA < b.PayloadSHA
	})
	return out
}

// Gaps finds holes inside one representation's ordered events and applies the
// recovery policy to each. Corrupt events are skipped: their duration is
// unknowable and they were already quarantined by ingest. quarantined maps the
// sequence numbers of corrupt packets so a hole they leave is attributed to
// corruption rather than to loss.
func Gaps(events []*model.Event, quarantined map[uint32]model.ErrorKind, maxFillUS int64) ([]recovery.Decision, recovery.Stats) {
	return GapsTo(events, quarantined, maxFillUS, 0)
}

// GapsTo is Gaps with a known media end. A tail shorter than mediaEndUS is a
// gap with no following packet to bound it, so it is reported unrecoverable
// rather than silently trimmed or filled with invented content.
func GapsTo(events []*model.Event, quarantined map[uint32]model.ErrorKind, maxFillUS, mediaEndUS int64) ([]recovery.Decision, recovery.Stats) {
	var decisions []recovery.Decision
	var stats recovery.Stats
	lastEnd := int64(0)
	for i := 0; i < len(events); i++ {
		cur := events[i]
		if cur.DurationUS <= 0 {
			continue
		}
		curEnd := cur.ExtTimeUS + cur.DurationUS
		if curEnd > lastEnd {
			lastEnd = curEnd
		}
		next := nextTimedEvent(events, i+1)
		if next == nil {
			continue
		}
		if next.ExtTimeUS <= curEnd {
			continue // overlapping or contiguous packets: no hole
		}
		if next.ID.Stream == cur.ID.Stream && next.ID.Seq != cur.ID.Seq+1 {
			reason := model.KindUnknownGap
			corruptOnly := true
			for seq := cur.ID.Seq + 1; seq < next.ID.Seq; seq++ {
				kind, ok := quarantined[seq]
				if !ok || kind != model.KindCorrupt {
					corruptOnly = false
					break
				}
			}
			if corruptOnly {
				reason = model.KindCorrupt
				stats.CorruptHoles++
			} else {
				stats.LossHoles++
			}
			d := recovery.Decide(recovery.Evidence{
				HoleSeq:       cur.ID.Seq + 1,
				HoleStartUS:   curEnd,
				PreviousEndUS: curEnd,
				NextStartUS:   next.ExtTimeUS,
				HasNext:       true,
				MaxFillUS:     maxFillUS,
				Reason:        reason,
			})
			if d.Recoverable {
				stats.EstimatedUS += d.DurationUS
				if reason == model.KindCorrupt {
					stats.RecoveredCorrupt++
				} else {
					stats.RecoveredLoss++
				}
			} else {
				stats.Unrecoverable++
			}
			decisions = append(decisions, d)
		}
	}
	if mediaEndUS > 0 && lastEnd > 0 && lastEnd < mediaEndUS {
		stats.LossHoles++
		stats.Unrecoverable++
		decisions = append(decisions, recovery.Decide(recovery.Evidence{
			HoleStartUS: lastEnd, PreviousEndUS: lastEnd, HasNext: false,
			Reason: model.KindUnknownGap,
		}))
	}
	return decisions, stats
}

func nextTimedEvent(events []*model.Event, from int) *model.Event {
	for i := from; i < len(events); i++ {
		if events[i].DurationUS > 0 {
			return events[i]
		}
	}
	return nil
}
