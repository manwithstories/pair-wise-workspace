package timeline

import (
	"testing"

	"mediascan/internal/media"
	"mediascan/internal/segment"
)

func ev(seq int, kind Kind, track uint32, startNs int64) Event {
	return Event{
		Seq: seq, Kind: kind, TrackID: track, StartNs: startNs, EndNs: startNs + 100_000_000,
		Recovery: segment.Clean,
		Source:   segment.Source{Kind: string(kind), Ref: "fixture"},
	}
}

func TestWatermarkCommitsClosedSegments(t *testing.T) {
	cfg := Config{SegmentNs: 1_000_000_000, TrackIDs: []uint32{1}}
	e := NewEngine(cfg)
	// Four events in the first second.
	for i := 0; i < 4; i++ {
		e.Add(ev(i, KindSubtitle, 1, int64(i)*100_000_000))
	}
	// Nothing is closed until the watermark passes the segment end.
	if got := len(e.Segments()); got != 0 {
		t.Fatalf("segments before watermark = %d, want 0", got)
	}
	e.AdvanceWatermark(1_000_000_000)
	segs := e.Segments()
	if len(segs) != 1 {
		t.Fatalf("segments = %d, want 1", len(segs))
	}
	if segs[0].StartNs != 0 || segs[0].EndNs != 1_000_000_000 {
		t.Errorf("segment bounds = %d..%d", segs[0].StartNs, segs[0].EndNs)
	}
	if len(segs[0].Events) != 4 {
		t.Errorf("events = %d, want 4", len(segs[0].Events))
	}
	if segs[0].Hash == "" {
		t.Error("segment hash empty")
	}
}

func TestWatermarkLateEventReplays(t *testing.T) {
	cfg := Config{SegmentNs: 1_000_000_000, TrackIDs: []uint32{1}, CheckpointEvery: 2}
	e := NewEngine(cfg)
	// Commit several segments on time.
	for s := 0; s < 4; s++ {
		for i := 0; i < 3; i++ {
			e.Add(ev(s*3+i, KindMedia, 1, int64(s)*1_000_000_000+int64(i)*200_000_000))
		}
		e.AdvanceWatermark(int64(s+1) * 1_000_000_000)
	}
	before := e.Segments()
	if len(before) != 4 {
		t.Fatalf("committed segments = %d, want 4", len(before))
	}
	// A late event lands in segment 2 after segments 2..3 were committed.
	late := ev(99, KindSubtitle, 2, 2_000_000_000+50_000_000)
	e.Add(late)
	if e.ReplayCount() < 1 {
		t.Fatal("late event did not trigger a replay")
	}
	if !e.ReplayEqual() {
		t.Error("replay did not reproduce its suffix exactly")
	}
	after := e.Segments()
	if len(after) != 4 {
		t.Fatalf("segments after replay = %d, want 4", len(after))
	}
	// Segment 2 now includes the late event.
	found := false
	for _, s := range after {
		for _, x := range s.Events {
			// The late event is the only subtitle event on track 2.
			if x.TrackID == 2 {
				found = true
				if s.Index != 2 {
					t.Errorf("late event landed in segment %d, want 2", s.Index)
				}
			}
		}
	}
	if !found {
		t.Error("late event missing from segments after replay")
	}
	// The prefix (segments 0..1) must be untouched.
	for i := 0; i < 2; i++ {
		if before[i].Hash != after[i].Hash {
			t.Errorf("prefix segment %d changed after replay", i)
		}
	}
	if !hasClass(e.Issues(), media.ClassTimelineLateEvent) {
		t.Errorf("late event issue missing: %v", e.Issues())
	}
	if !hasClass(e.Issues(), media.ClassTimelineReplay) {
		t.Errorf("replay issue missing: %v", e.Issues())
	}
}

func TestLateEventDeterministicAcrossArrivalOrders(t *testing.T) {
	cfg := Config{SegmentNs: 1_000_000_000, TrackIDs: []uint32{1, 2}, CheckpointEvery: 3}
	var events []Event
	for s := 0; s < 6; s++ {
		for i := 0; i < 2; i++ {
			events = append(events, ev(s*2+i, KindMedia, uint32(1+i), int64(s)*1_000_000_000+int64(i)*300_000_000))
		}
	}
	if !VerifyReplay(events, cfg) {
		t.Fatal("segmenter is order-dependent: reversed arrival produced different segments")
	}
}

func TestLateEventChangesHashOnlyWhereItLands(t *testing.T) {
	cfg := Config{SegmentNs: 1_000_000_000, TrackIDs: []uint32{1}, CheckpointEvery: 2}
	build := func(withLate bool) []Segment {
		e := NewEngine(cfg)
		for s := 0; s < 4; s++ {
			e.Add(ev(s, KindMedia, 1, int64(s)*1_000_000_000+100_000_000))
			e.AdvanceWatermark(int64(s+1) * 1_000_000_000)
		}
		if withLate {
			e.Add(ev(50, KindSubtitle, 1, 3_000_000_000+500_000_000))
		}
		return e.Segments()
	}
	plain := build(false)
	withLate := build(true)
	for i := 0; i < 3; i++ {
		if plain[i].Hash != withLate[i].Hash {
			t.Errorf("segment %d hash changed though event landed later", i)
		}
	}
	if plain[3].Hash == withLate[3].Hash {
		t.Error("segment 3 hash did not change after receiving a late event")
	}
}

func TestWatermarkNegativeIgnored(t *testing.T) {
	e := NewEngine(Config{})
	e.AdvanceWatermark(-5)
	if e.Watermark() != 0 {
		t.Errorf("watermark = %d, want 0", e.Watermark())
	}
	if !hasClass(e.Issues(), media.ClassTimelineWatermarkTime) {
		t.Errorf("negative watermark not reported: %v", e.Issues())
	}
}

func hasClass(set media.IssueSet, c media.Class) bool {
	for _, is := range set {
		if is.Class == c {
			return true
		}
	}
	return false
}
