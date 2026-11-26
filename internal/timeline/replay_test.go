package timeline

import (
	"testing"

	"mediad/internal/media"
)

func TestAddOnTimeOrderingStable(t *testing.T) {
	tl := NewDefault()
	evs := []Event{
		{Time: 100, Camera: "b", Track: "t1", Seq: 1},
		{Time: 50, Camera: "a", Track: "t1", Seq: 2},
		{Time: 100, Camera: "a", Track: "t1", Seq: 3},
		{Time: 100, Camera: "a", Track: "t0", Seq: 4},
	}
	for _, e := range evs {
		if r := tl.Add(e); r != 0 {
			t.Fatalf("unexpected replay %d", r)
		}
	}
	got := tl.Events()
	if len(got) != 4 {
		t.Fatalf("len=%d", len(got))
	}
	// 50/a/t1, 100/a/t0, 100/a/t1, 100/b/t1
	if got[0].Time != 50 || got[1].Track != "t0" || got[2].Camera != "a" || got[3].Camera != "b" {
		t.Fatalf("order: %+v", got)
	}
	if tl.Watermark() != 100 {
		t.Fatalf("watermark=%d", tl.Watermark())
	}
}

func TestLateTriggersReplayAndLateness(t *testing.T) {
	tl := New(LatenessWindow, 2)
	tl.Add(Event{Time: 10 * 48000, Camera: "a", Track: "t", Seq: 1})
	tl.Add(Event{Time: 20 * 48000, Camera: "a", Track: "t", Seq: 2})
	// This arrives more than 2 s behind the watermark (20s) -> late.
	replayed := tl.Add(Event{Time: 12 * 48000, Camera: "a", Track: "t", Seq: 3})
	if replayed == 0 {
		t.Fatal("expected a replay")
	}
	if tl.LateEvents() != 1 {
		t.Fatalf("late=%d", tl.LateEvents())
	}
	if tl.Watermark() != 20*48000 {
		t.Fatalf("watermark rewound: %d", tl.Watermark())
	}
	got := tl.Events()
	if len(got) != 3 || got[1].Seq != 3 {
		t.Fatalf("events: %+v", got)
	}
}

func TestWithinWindowNoReplay(t *testing.T) {
	tl := NewDefault()
	tl.Add(Event{Time: 0, Camera: "a", Track: "t", Seq: 1})
	tl.Add(Event{Time: 1 * 48000, Camera: "a", Track: "t", Seq: 2})
	// 0.5 s behind: within the 2 s window, so on time.
	if r := tl.Add(Event{Time: 5000, Camera: "a", Track: "t", Seq: 3}); r != 0 {
		t.Fatalf("replay=%d", r)
	}
	if tl.LateEvents() != 0 {
		t.Fatal("should not be late")
	}
}

func TestReplayDeterministicAgainstCleanRun(t *testing.T) {
	// Order A: with a late arrival. Order B: all events known up front.
	all := []Event{
		{Time: 1 * 48000, Camera: "a", Track: "x", Seq: 1},
		{Time: 9 * 48000, Camera: "b", Track: "x", Seq: 2},
		{Time: 17 * 48000, Camera: "c", Track: "x", Seq: 3},
		{Time: 5 * 48000, Camera: "d", Track: "x", Seq: 4}, // late
	}
	tlA := New(LatenessWindow, 2)
	for i, e := range all {
		if i == 3 {
			if r := tlA.Add(e); r == 0 {
				t.Fatal("expected replay")
			}
			continue
		}
		tlA.Add(e)
	}
	tlB := New(LatenessWindow, 2)
	// Sorted clean insertion.
	clean := []Event{all[0], all[3], all[1], all[2]}
	for _, e := range clean {
		tlB.Add(e)
	}
	a, b := tlA.Events(), tlB.Events()
	if len(a) != len(b) {
		t.Fatalf("len %d vs %d", len(a), len(b))
	}
	for i := range a {
		if Compare(a[i], b[i]) != 0 {
			t.Fatalf("event %d differs: %+v vs %+v", i, a[i], b[i])
		}
	}
}

// TestDeepReplayAcrossManySnapshots ensures a late event that sorts before
// several snapshots still yields the clean-run order.
func TestDeepReplayAcrossManySnapshots(t *testing.T) {
	tl := New(LatenessWindow, 2)
	var onTime []Event
	for i := 0; i < 40; i++ {
		e := Event{Time: media.MediaTick((i + 5) * 48000), Camera: "a", Track: "t", Seq: int64(i)}
		onTime = append(onTime, e)
		tl.Add(e)
	}
	late := Event{Time: 1 * 48000, Camera: "b", Track: "t", Seq: 999}
	if r := tl.Add(late); r == 0 {
		t.Fatal("expected replay")
	}
	clean := New(LatenessWindow, 2)
	clean.Add(late)
	for _, e := range onTime {
		clean.Add(e)
	}
	a, b := tl.Events(), clean.Events()
	if len(a) != len(b) {
		t.Fatalf("len %d vs %d", len(a), len(b))
	}
	for i := range a {
		if Compare(a[i], b[i]) != 0 {
			t.Fatalf("diff at %d: %+v vs %+v", i, a[i], b[i])
		}
	}
}

// TestWatermarkNeverRewinds confirms replays keep event time monotonic.
func TestWatermarkNeverRewinds(t *testing.T) {
	tl := New(LatenessWindow, 1)
	tl.Add(Event{Time: 100 * 48000, Camera: "a", Track: "t", Seq: 1})
	mark := tl.Watermark()
	tl.Add(Event{Time: 1 * 48000, Camera: "b", Track: "t", Seq: 2}) // very late
	if tl.Watermark() < mark {
		t.Fatalf("watermark rewound: %d < %d", tl.Watermark(), mark)
	}
}
