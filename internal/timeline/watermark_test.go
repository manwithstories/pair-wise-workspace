package timeline

import (
	"testing"

	"mediameld/internal/model"
)

func ev(track string, seq uint32, idx int, ptsMs int64) model.Event {
	return model.Event{
		TrackID: track, Seq: seq, Index: idx,
		PTS: model.Instant(ptsMs) * 1_000_000, Duration: 1_000_000,
		Hash: h(byte(idx + 1)), Kind: model.KindVideo,
	}
}

func h(b byte) [32]byte {
	var x [32]byte
	x[0] = b
	return x
}

func TestSegmentClosedIntervalBoundary(t *testing.T) {
	span := model.Instant(1000)
	// Instant 999 and 1000 are adjacent segments; 1000 must belong only to 1.
	if SegmentFor(999, span) != 0 || SegmentFor(1000, span) != 1 {
		t.Fatalf("segment boundaries wrong: %d %d", SegmentFor(999, span), SegmentFor(1000, span))
	}
	if !InSegment(1000, 1, span) {
		t.Fatal("1000 not in segment 1")
	}
	if InSegment(1000, 0, span) {
		t.Fatal("1000 wrongly also in segment 0")
	}
	// An event at the very end of segment 0 remains in segment 0.
	if !InSegment(1999, 1, span) || InSegment(1999, 2, span) {
		t.Fatal("upper closed boundary miscounted")
	}
}

func TestWatermarkIsMaxMinusLate(t *testing.T) {
	cfg := Config{AllowedLate: 250_000_000, SegmentSpan: 1_000_000_000}
	events := []model.Event{
		ev("v", 1, 0, 100),
		ev("v", 2, 1, 200),
		ev("v", 3, 2, 900),
	}
	plan, err := Build(events, cfg)
	if err != nil {
		t.Fatal(err)
	}
	// The latest event ends at 901ms (900ms PTS + 1ms), minus the 250ms
	// late window, giving a watermark of 651ms.
	want := model.Instant(651_000_000)
	if plan.Watermark != want {
		t.Fatalf("watermark=%v want %v", plan.Watermark, want)
	}
}

func TestBuildDeterministic(t *testing.T) {
	cfg := Config{AllowedLate: 200_000_000, SegmentSpan: 500_000_000}
	var events []model.Event
	for i := 0; i < 60; i++ {
		events = append(events, ev("v", uint32(i+1), i, int64(i)*33))
	}
	p1, err := Build(events, cfg)
	if err != nil {
		t.Fatal(err)
	}
	// Shuffle the input order; the hash must be identical.
	reordered := append([]model.Event(nil), events...)
	for i := 0; i < len(reordered); i += 2 {
		j := (i * 7) % len(reordered)
		reordered[i], reordered[j] = reordered[j], reordered[i]
	}
	p2, err := Build(reordered, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if p1.Hash != p2.Hash {
		t.Fatalf("hash not deterministic:\n %s\n %s", p1.Hash, p2.Hash)
	}
	if len(p1.Hash) != 64 {
		t.Fatalf("hash length %d", len(p1.Hash))
	}
}

func TestGapsFromMissingSequence(t *testing.T) {
	cfg := Config{AllowedLate: 0, SegmentSpan: 1_000_000_000}
	events := []model.Event{
		ev("v", 1, 0, 0), ev("v", 2, 1, 33),
		ev("v", 5, 4, 132), // 3 and 4 missing
	}
	plan, err := Build(events, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Gaps != 2 {
		t.Fatalf("gaps=%d want 2", plan.Gaps)
	}
	// Gap markers must be explicit events in the timeline.
	gaps := 0
	for _, e := range plan.Events {
		if e.IsGap {
			gaps++
		}
	}
	if gaps != 2 {
		t.Fatalf("gap events=%d", gaps)
	}
}

func TestDuplicateSequenceNotDuplicated(t *testing.T) {
	cfg := Config{AllowedLate: 0, SegmentSpan: 1_000_000_000}
	e := ev("v", 7, 6, 100)
	plan, err := Build([]model.Event{e, e, e}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Events) != 1 {
		t.Fatalf("events=%d want 1 after dedup", len(plan.Events))
	}
}

func TestLateEventCountsReplay(t *testing.T) {
	cfg := Config{AllowedLate: 50_000_000, SegmentSpan: 1_000_000_000}
	// Arrival order: 0ms, 900ms, then 500ms (late beyond the 50ms window).
	events := []model.Event{
		ev("v", 1, 0, 0),
		ev("v", 2, 1, 900),
		ev("v", 3, 2, 500),
	}
	plan, err := Build(events, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if plan.LateReplays != 1 {
		t.Fatalf("late_replays=%d want 1", plan.LateReplays)
	}
	// The late event still lands at its true PTS, sorted before 900ms.
	var order []int64
	for _, e := range plan.Events {
		order = append(order, int64(e.PTS)/1_000_000)
	}
	want := []int64{0, 500, 900}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("order=%v want %v", order, want)
		}
	}
}

func TestReplayerRewindDeterministic(t *testing.T) {
	cfg := Config{AllowedLate: 40_000_000, SegmentSpan: 100_000_000}
	r, err := NewReplayer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	arrivals := []model.Event{
		ev("v", 1, 0, 0), ev("v", 2, 1, 400), ev("v", 3, 2, 600),
		ev("v", 4, 3, 200), // late
	}
	for _, e := range arrivals {
		r.Push(e)
	}
	plan := r.Finalize()
	if plan.LateReplays != 1 {
		t.Fatalf("replays=%d want 1", plan.LateReplays)
	}
	// Re-running produces the same hash.
	r2, _ := NewReplayer(cfg)
	for _, e := range arrivals {
		r2.Push(e)
	}
	if r2.Finalize().Hash != plan.Hash {
		t.Fatal("replay hash not stable across runs")
	}
}

func TestReplayerIgnoresDuplicatePacket(t *testing.T) {
	cfg := Config{AllowedLate: 0, SegmentSpan: 1_000_000_000}
	r, _ := NewReplayer(cfg)
	e := ev("v", 1, 0, 0)
	r.Push(e)
	r.Push(e)
	if len(r.Events()) != 1 {
		t.Fatalf("duplicate packet re-entered samples: %d", len(r.Events()))
	}
}

func TestInvalidConfig(t *testing.T) {
	if _, err := Build(nil, Config{SegmentSpan: 0}); err == nil {
		t.Fatal("zero span accepted")
	}
	if _, err := Build(nil, Config{SegmentSpan: 1, AllowedLate: -1}); err == nil {
		t.Fatal("negative late window accepted")
	}
}

func TestSegmentsCoverAllEventsOnce(t *testing.T) {
	cfg := Config{AllowedLate: 10_000_000, SegmentSpan: 250_000_000}
	var events []model.Event
	for i := 0; i < 40; i++ {
		events = append(events, ev("v", uint32(i+1), i, int64(i)*20))
	}
	plan, _ := Build(events, cfg)
	seen := 0
	for _, seg := range plan.Segments {
		for _, e := range seg.Events {
			if !InSegment(e.PTS, seg.Index, cfg.SegmentSpan) {
				t.Fatalf("event %d in wrong segment %d", e.Index, seg.Index)
			}
			seen++
		}
	}
	if seen != len(plan.Events) {
		t.Fatalf("segments cover %d of %d events", seen, len(plan.Events))
	}
}

func TestWatermarkUsesEventEnd(t *testing.T) {
	// A short event at 900ms and a long event ending at 900ms must agree:
	// only the end time, not the PTS, drives the watermark.
	cfg := Config{AllowedLate: 0, SegmentSpan: 1_000_000_000}
	long := model.Event{TrackID: "a", Seq: 1, Index: 0, PTS: 800_000_000, Duration: 100_000_000}
	short := model.Event{TrackID: "b", Seq: 1, Index: 0, PTS: 900_000_000, Duration: 0}
	plan, _ := Build([]model.Event{long, short}, cfg)
	if plan.Watermark != 900_000_000 {
		t.Fatalf("watermark=%v want 900ms", plan.Watermark)
	}
}

func TestCheckpointIndex(t *testing.T) {
	span := model.Instant(1000)
	if CheckpointIndex(2500, span) != 1 {
		t.Fatalf("checkpoint=%d want 1", CheckpointIndex(2500, span))
	}
	if CheckpointIndex(0, span) != 0 {
		t.Fatalf("checkpoint at zero=%d", CheckpointIndex(0, span))
	}
}
