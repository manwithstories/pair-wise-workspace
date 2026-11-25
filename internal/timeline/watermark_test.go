package timeline

import (
	"fmt"
	"testing"

	"mediapipe/internal/media"
)

func newEngine(t *testing.T) *Engine {
	t.Helper()
	e, err := New(DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func ev(key string, at media.Timestamp) Event {
	return Event{Key: key, Track: 1, Time: at, Duration: media.Millisecond, Payload: []byte(key)}
}

func TestWatermarkTrailsMaxLateness(t *testing.T) {
	e := newEngine(t)
	if _, _, err := e.Push(ev("a", 5*media.Second)); err != nil {
		t.Fatal(err)
	}
	want := 5*media.Second - 250*media.Millisecond
	if e.Watermark() != want {
		t.Fatalf("watermark=%d want %d", e.Watermark(), want)
	}
	if _, _, err := e.Push(ev("b", 3*media.Second)); err != nil {
		t.Fatal(err)
	}
	if e.Watermark() != want {
		t.Fatalf("watermark regressed to %d", e.Watermark())
	}
}

func TestLateWithinHorizonRecomputes(t *testing.T) {
	e := newEngine(t)
	// Advance to 5s, watermark at 4.75s.
	if _, _, err := e.Push(ev("a", 5*media.Second)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.Push(ev("b", 4*media.Second)); err != nil {
		t.Fatal(err)
	}
	// 4s window is closed (watermark 4.75s) but only 750ms late, so it is
	// accepted and the window is marked recomputed.
	accepted, late, err := e.Push(ev("c", 4*media.Second))
	if err != nil {
		t.Fatal(err)
	}
	if !accepted || !late {
		t.Fatalf("accepted=%v late=%v", accepted, late)
	}
	if e.Stats().Recomputes != 1 {
		t.Fatalf("recomputes=%d", e.Stats().Recomputes)
	}
	sums := e.Summaries()
	var found bool
	for _, s := range sums {
		if s.Start == 4*media.Second && s.Recomputed && s.Events == 2 {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected recomputed 4s window: %+v", sums)
	}
}

func TestBeyondRetractionQuarantines(t *testing.T) {
	e := newEngine(t)
	if _, _, err := e.Push(ev("a", 10*media.Second)); err != nil {
		t.Fatal(err)
	}
	// Watermark 9.75s; an event at 5s is 4.75s behind, beyond 2s.
	accepted, _, err := e.Push(ev("z", 5*media.Second))
	if err == nil {
		t.Fatal("expected quarantine error")
	}
	if accepted {
		t.Fatal("quarantined event must not be accepted")
	}
	if media.ClassOf(err) != media.ClassUnsupported {
		t.Fatalf("class=%v", media.ClassOf(err))
	}
	if media.ExitCode(err) != 3 {
		t.Fatalf("exit=%d", media.ExitCode(err))
	}
	if len(e.Quarantine()) != 1 || e.Quarantine()[0].Event.Key != "z" {
		t.Fatalf("quarantine=%+v", e.Quarantine())
	}
}

func TestExactlyAtHorizonAccepted(t *testing.T) {
	e := newEngine(t)
	if _, _, err := e.Push(ev("a", 10*media.Second)); err != nil {
		t.Fatal(err)
	}
	// watermark = 9.75s; event at 7.75s is exactly 2s behind.
	_, late, err := e.Push(ev("edge", 7_750*media.Millisecond))
	if err != nil {
		t.Fatalf("edge event at horizon rejected: %v", err)
	}
	if !late {
		t.Fatal("edge event should be late")
	}
}

func TestDuplicateKeysIgnored(t *testing.T) {
	e := newEngine(t)
	if _, _, err := e.Push(ev("a", 1*media.Second)); err != nil {
		t.Fatal(err)
	}
	accepted, _, err := e.Push(ev("a", 1*media.Second))
	if err != nil {
		t.Fatal(err)
	}
	if accepted {
		t.Fatal("duplicate accepted")
	}
	if e.Stats().Duplicates != 1 {
		t.Fatalf("duplicates=%d", e.Stats().Duplicates)
	}
	if sums := e.Summaries(); len(sums) != 1 || sums[0].Events != 1 {
		t.Fatalf("summaries=%+v", sums)
	}
}

func TestReplayDigestStableAcrossRuns(t *testing.T) {
	mk := func() [32]byte {
		e := newEngine(t)
		in := []Event{
			ev("a", 1*media.Second),
			ev("b", 1_500*media.Millisecond),
			ev("c", 2*media.Second),
			ev("a", 1*media.Second), // duplicate
			ev("d", 1_200*media.Millisecond), // late but inside horizon
		}
		for _, x := range in {
			if _, _, err := e.Push(x); err != nil {
				t.Fatal(err)
			}
		}
		return e.ReplayDigest()
	}
	first := mk()
	for i := 0; i < 5; i++ {
		if got := mk(); got != first {
			t.Fatalf("run %d digest differs", i)
		}
	}
}

func TestLateEventChangesManifestTimeline(t *testing.T) {
	// Without late data the timeline starts at 2s; a late event at 1.75s
	// (inside the 2s retraction horizon behind the 3.75s watermark) must move
	// the start earlier, proving recompute affects the manifest time range.
	build := func(withLate bool) (media.Timestamp, media.Timestamp) {
		e := newEngine(t)
		for _, at := range []media.Timestamp{2 * media.Second, 3 * media.Second, 4 * media.Second} {
			if _, _, err := e.Push(ev(fmt.Sprintf("%d", at), at)); err != nil {
				t.Fatal(err)
			}
		}
		if withLate {
			if _, _, err := e.Push(ev("late", 1_750*media.Millisecond)); err != nil {
				t.Fatal(err)
			}
		}
		return e.Timeline()
	}
	s0, _ := build(false)
	s1, _ := build(true)
	if s0 != 2*media.Second {
		t.Fatalf("clean start=%d", s0)
	}
	if s1 != 1*media.Second {
		t.Fatalf("late start=%d want 1s", s1)
	}
}

func TestSummaryDigestIndependentOfArrivalOrder(t *testing.T) {
	// Two engines receiving the same events in different orders must produce
	// the same window summaries.
	a := newEngine(t)
	b := newEngine(t)
	order := []media.Timestamp{
		1 * media.Second, 1_100 * media.Millisecond, 2 * media.Second,
	}
	for _, at := range order {
		if _, _, err := a.Push(ev(fmt.Sprintf("%d", at), at)); err != nil {
			t.Fatal(err)
		}
	}
	for i := len(order) - 1; i >= 0; i-- {
		at := order[i]
		if _, _, err := b.Push(ev(fmt.Sprintf("%d", at), at)); err != nil {
			t.Fatal(err)
		}
	}
	sa, sb := a.Summaries(), b.Summaries()
	if len(sa) != len(sb) {
		t.Fatalf("window counts differ: %d vs %d", len(sa), len(sb))
	}
	for i := range sa {
		if sa[i].Digest != sb[i].Digest || sa[i].Events != sb[i].Events {
			t.Fatalf("window %d differs", i)
		}
	}
}

func TestConfigValidation(t *testing.T) {
	if _, err := New(Config{Window: 0}); err == nil {
		t.Fatal("expected window error")
	}
	if _, err := New(Config{MaxLateness: 2 * media.Second, Retraction: media.Second, Window: media.Second}); err == nil {
		t.Fatal("expected retraction shorter than lateness error")
	}
}

func TestTimelineRangeCoversDurations(t *testing.T) {
	e := newEngine(t)
	x := ev("a", 500*media.Millisecond)
	x.Duration = 700 * media.Millisecond
	if _, _, err := e.Push(x); err != nil {
		t.Fatal(err)
	}
	start, end := e.Timeline()
	if start != 0 {
		t.Fatalf("start=%d", start)
	}
	if end < 1200*media.Millisecond {
		t.Fatalf("end=%d does not cover packet end", end)
	}
}

func TestQuarantineBlocksCommitSemantics(t *testing.T) {
	// The engine surfaces the first quarantine error; callers must refuse to
	// commit even if later events are fine.
	e := newEngine(t)
	if _, _, err := e.Push(ev("a", 10*media.Second)); err != nil {
		t.Fatal(err)
	}
	_, _, err := e.Push(ev("old", media.Second))
	if err == nil {
		t.Fatal("expected quarantine")
	}
	if _, _, err := e.Push(ev("b", 11*media.Second)); err != nil {
		t.Fatalf("later event should still be accepted: %v", err)
	}
	if e.Stats().Quarantined != 1 {
		t.Fatalf("quarantined=%d", e.Stats().Quarantined)
	}
}
