package clock

import (
	"testing"

	"medpipe/internal/model"
)

func TestNormalizeMonotonic(t *testing.T) {
	tl := New(DefaultConfig())
	for i := 0; i < 10; i++ {
		got, err := tl.Normalize(Video, uint32(i), int64(i)*40_000_000)
		if err != nil || got != int64(i)*40_000_000 {
			t.Fatalf("i=%d got=%d err=%v", i, got, err)
		}
	}
}

func TestWrapAround(t *testing.T) {
	tl := New(DefaultConfig())
	period := int64(1) << 32
	base := period - 60
	first, err := tl.Normalize(Video, 0, base)
	if err != nil {
		t.Fatal(err)
	}
	// Next timestamp wraps to a small raw value.
	second, err := tl.Normalize(Video, 1, 20)
	if err != nil {
		t.Fatalf("wrap must be repaired: %v", err)
	}
	if second != first+80 {
		t.Fatalf("wrap math wrong: %d vs %d", second, first+80)
	}
	if tl.State(Video).Wraps != 1 {
		t.Fatal("wrap not counted")
	}
}

func TestDuplicateSequence(t *testing.T) {
	tl := New(DefaultConfig())
	a, _ := tl.Normalize(Audio, 5, 100)
	b, err := tl.Normalize(Audio, 5, 100)
	if err != nil || a != b {
		t.Fatalf("identical duplicate must resolve: %v", err)
	}
	if tl.State(Audio).Duplicates != 1 {
		t.Fatal("duplicate not counted")
	}
	// Same seq, conflicting timestamp: ambiguous -> track stops.
	if _, err := tl.Normalize(Audio, 5, 999); err == nil {
		t.Fatal("conflicting duplicate must stop the track")
	}
	st := tl.State(Audio)
	if !st.Stopped || st.Failure.Code != model.ErrTimeline {
		t.Fatalf("track state wrong: %+v", st)
	}
	// Stopped tracks keep returning the same failure.
	if _, err := tl.Normalize(Audio, 6, 200); err == nil {
		t.Fatal("stopped track must reject further input")
	}
}

func TestBoundedReorder(t *testing.T) {
	tl := New(DefaultConfig())
	for i := 0; i < 8; i++ {
		if _, err := tl.Normalize(Video, uint32(i), int64(i)*10); err != nil {
			t.Fatal(err)
		}
	}
	// seq 6 arrives again inside the window: resolvable.
	if _, err := tl.Normalize(Video, 6, 60); err != nil {
		t.Fatalf("in-window reorder should resolve: %v", err)
	}
	// seq 1 is far behind the head: ambiguous.
	if _, err := tl.Normalize(Video, 1, 10); err == nil {
		t.Fatal("out-of-window late seq must stop the track")
	}
}

func TestNonMonotonicStopsTrack(t *testing.T) {
	tl := New(Config{WrapPeriodNS: 0, MaxReorderSeq: 4}) // wrap disabled
	tl.Normalize(Video, 0, 100)
	_, err := tl.Normalize(Video, 1, 50)
	if err == nil {
		t.Fatal("backwards time without wrap must stop track")
	}
	st := tl.State(Video)
	if !st.Stopped || st.AffectedStartNS != 50 || st.AffectedEndNS != 100 {
		t.Fatalf("affected range wrong: %+v", st)
	}
	tl.Finalize(Video, 200)
	if tl.State(Video).AffectedEndNS != 200 {
		t.Fatal("finalize must close the affected range")
	}
}

func TestAVOffset(t *testing.T) {
	if AVOffset(100, 115) != 15 || AVOffset(115, 100) != 15 {
		t.Fatal("AV offset must be absolute")
	}
}
