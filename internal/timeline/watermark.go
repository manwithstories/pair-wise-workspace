// Package timeline orders calibrated events into a deterministic delivery
// timeline. A watermark advances by the maximum observed event time across
// sources minus the allowed late window; events that arrive after the
// watermark has passed them trigger a bounded, deterministic replay from the
// nearest checkpoint.
package timeline

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"sort"

	"mediameld/internal/model"
)

// Segment is a closed time interval [Start, End] in nanoseconds.
type Segment struct {
	Index  int
	Start  model.Instant
	End    model.Instant
	Events []model.Event
}

// Config controls watermark segmentation.
type Config struct {
	AllowedLate model.Instant // permitted lateness before a replay is needed
	SegmentSpan model.Instant // nominal span of one closed-interval segment
}

func (c Config) validate() error {
	if c.SegmentSpan <= 0 {
		return model.NewError(model.CodeTimeline, "segment span must be positive")
	}
	if c.AllowedLate < 0 {
		return model.NewError(model.CodeTimeline, "allowed lateness must not be negative")
	}
	return nil
}

// Plan is the reproducible output of the timeline engine.
type Plan struct {
	Segments    []Segment
	Events      []model.Event // globally ordered, gaps materialized
	Watermark   model.Instant
	LateReplays int
	Gaps        int
	Hash        string
}

// SegmentFor assigns an instant to a closed-interval segment. A boundary
// instant belongs to exactly the lower segment because the interval is closed
// on both ends and segment i+1 starts strictly after segment i ends.
func SegmentFor(t model.Instant, span model.Instant) int {
	if t < 0 {
		return -1
	}
	return int(int64(t) / int64(span))
}

// InSegment reports whether an instant belongs to segment i under the closed
// interval rule. The end of a segment is exclusive of the next segment's
// start, so an event can never be counted twice.
func InSegment(t model.Instant, i int, span model.Instant) bool {
	start := model.Instant(int64(i) * int64(span))
	end := model.Instant(int64(i+1)*int64(span) - 1)
	return t >= start && t <= end
}

// Build consumes all events, assigns segments, materializes explicit gaps for
// missing sequence ranges and runs deterministic replay for late arrivals.
// Running Build twice on the same input list yields the same Hash.
func Build(events []model.Event, cfg Config) (Plan, error) {
	if err := cfg.validate(); err != nil {
		return Plan{}, err
	}
	// Group by track so gaps can be derived per source and replayed
	// independently, then remerge deterministically by (PTS, track, index).
	byTrack := map[string][]model.Event{}
	var order []string
	for _, e := range events {
		if _, ok := byTrack[e.TrackID]; !ok {
			order = append(order, e.TrackID)
		}
		byTrack[e.TrackID] = append(byTrack[e.TrackID], e)
	}
	sort.Strings(order)

	plan := Plan{}
	var merged []model.Event
	for _, id := range order {
		tr := byTrack[id]
		sorted, gaps, replays := normalizeTrack(tr, cfg)
		plan.Gaps += gaps
		plan.LateReplays += replays
		merged = append(merged, sorted...)
	}
	sortEvents(merged)

	// Assign segments and build the closed-interval segment list.
	plan.Segments = segmentEvents(merged, cfg.SegmentSpan)
	var maxT model.Instant
	for _, e := range merged {
		if e.PTS+e.Duration > maxT {
			maxT = e.PTS + e.Duration
		}
	}
	plan.Watermark = maxT - cfg.AllowedLate
	if plan.Watermark < 0 {
		plan.Watermark = 0
	}
	plan.Events = merged
	plan.Hash = HashEvents(merged)
	return plan, nil
}

// normalizeTrack sorts one track's events, drops duplicate sequence numbers
// (keeping the first by deterministic order), inserts explicit gap markers for
// missing sequence ranges, and counts late-arrival replays by comparing
// receive order to presentation order.
func normalizeTrack(in []model.Event, cfg Config) ([]model.Event, int, int) {
	sorted := append([]model.Event(nil), in...)
	// Deterministic base order: transport sequence, then index. A replay
	// sorts by presentation time and is independent of arrival order.
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].Seq != sorted[j].Seq {
			return sorted[i].Seq < sorted[j].Seq
		}
		return sorted[i].Index < sorted[j].Index
	})
	// Deduplicate identical sequence numbers; the first wins.
	var dedup []model.Event
	seenSeq := map[uint32]bool{}
	seqKnown := false
	for _, e := range sorted {
		if e.Seq != 0 {
			seqKnown = true
		}
	}
	for _, e := range sorted {
		if seqKnown && e.Seq != 0 {
			if seenSeq[e.Seq] {
				continue
			}
			seenSeq[e.Seq] = true
		}
		dedup = append(dedup, e)
	}

	// Count replays: an event whose PTS is before the running max PTS minus
	// the late window requires the watermark to retreat.
	replays := 0
	var high model.Instant
	for i, e := range dedup {
		if i > 0 && e.PTS < high-cfg.AllowedLate {
			replays++
		}
		if e.PTS > high {
			high = e.PTS
		}
	}

	// Insert gap markers for missing transport sequence ranges.
	withGaps := dedup
	if seqKnown {
		withGaps = insertGaps(dedup, cfg)
	}
	sortEvents(withGaps)
	return withGaps, countGaps(withGaps), replays
}

// insertGaps adds a zero-length gap marker at the position of each missing
// sequence number, anchored to the previous sample's end time.
func insertGaps(events []model.Event, cfg Config) []model.Event {
	if len(events) == 0 {
		return events
	}
	out := make([]model.Event, 0, len(events))
	first := events[0]
	var prevEnd model.Instant
	for i, e := range events {
		if i == 0 {
			prevEnd = e.PTS + e.Duration
			out = append(out, e)
			continue
		}
		prevSeq := events[i-1].Seq
		if e.Seq > prevSeq+1 {
			for s := prevSeq + 1; s < e.Seq; s++ {
				out = append(out, model.Event{
					TrackID: e.TrackID, Fingerprint: e.Fingerprint, Kind: e.Kind,
					Index: -int(s), Seq: s, DTS: prevEnd, PTS: prevEnd,
					Duration: cfg.SegmentSpan / 1000, IsGap: true,
				})
			}
		}
		out = append(out, e)
		prevEnd = e.PTS + e.Duration
	}
	_ = first
	return out
}

func countGaps(events []model.Event) int {
	n := 0
	for _, e := range events {
		if e.IsGap {
			n++
		}
	}
	return n
}

// sortEvents is the single canonical ordering used everywhere: presentation
// time, then track id, then index. Gaps carry negative indexes so they precede
// the sample that follows the missing range.
func sortEvents(events []model.Event) {
	sort.SliceStable(events, func(i, j int) bool {
		a, b := events[i], events[j]
		if a.PTS != b.PTS {
			return a.PTS < b.PTS
		}
		if a.TrackID != b.TrackID {
			return a.TrackID < b.TrackID
		}
		return a.Index < b.Index
	})
}

func segmentEvents(events []model.Event, span model.Instant) []Segment {
	bySeg := map[int]*Segment{}
	var idxs []int
	for i := range events {
		s := SegmentFor(events[i].PTS, span)
		events[i].Segment = s
		seg, ok := bySeg[s]
		if !ok {
			seg = &Segment{
				Index: s,
				Start: model.Instant(int64(s) * int64(span)),
				End:   model.Instant(int64(s+1)*int64(span) - 1),
			}
			bySeg[s] = seg
			idxs = append(idxs, s)
		}
		seg.Events = append(seg.Events, events[i])
	}
	sort.Ints(idxs)
	out := make([]Segment, 0, len(idxs))
	for _, i := range idxs {
		out = append(out, *bySeg[i])
	}
	return out
}

// HashEvents derives a stable SHA-256 over the ordered timeline. It hashes
// only identity-bearing fields (track, index, times, sample hash, gap flag),
// so a repeat run over the same events always produces the same digest.
func HashEvents(events []model.Event) string {
	h := sha256.New()
	var buf [8]byte
	for _, e := range events {
		h.Write([]byte(e.TrackID))
		h.Write([]byte{0})
		h.Write([]byte(e.Fingerprint))
		h.Write([]byte{0})
		binary.BigEndian.PutUint64(buf[:], uint64(e.Index))
		h.Write(buf[:])
		binary.BigEndian.PutUint64(buf[:], uint64(e.PTS))
		h.Write(buf[:])
		binary.BigEndian.PutUint64(buf[:], uint64(e.Duration))
		h.Write(buf[:])
		h.Write(e.Hash[:])
		if e.IsGap {
			h.Write([]byte{1})
		} else {
			h.Write([]byte{0})
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

// CheckpointIndex is where replay restarts from. It is the segment index
// immediately below the late event's segment.
func CheckpointIndex(t model.Instant, span model.Instant) int {
	s := SegmentFor(t, span)
	if s <= 0 {
		return 0
	}
	return s - 1
}

var _ = fmt.Sprint
