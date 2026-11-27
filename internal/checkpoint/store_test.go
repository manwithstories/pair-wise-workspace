package checkpoint

import (
	"os"
	"path/filepath"
	"testing"

	"mediad/internal/media"
	"mediad/internal/recovery"
)

func mkState(gen uint64, arrival int64) *State {
	return &State{
		Generation:   gen,
		Arrival:      arrival,
		Watermark:    12345,
		WatermarkSet: true,
		OutputOffset: 6789,
		Tracks: []TrackState{
			{SSRC: 7, ClockRate: 48000, Expected: 100, Missing: []uint64{97, 98},
				Stats: recoveryStats(), ResamplerPos: 3},
		},
		Counters: Counters{FirstFault: 42, Loss: 2},
	}
}

func recoveryStats() (s recovery.Stats) {
	s.Lost = 2
	s.GapFills = 5
	return
}

func TestRoundTrip(t *testing.T) {
	dir := t.TempDir()
	st := New(filepath.Join(dir, "ck"))
	if err := st.Save(mkState(1, 5000)); err != nil {
		t.Fatal(err)
	}
	got, fb, err := st.Load()
	if err != nil || fb != nil {
		t.Fatalf("load: fb=%v err=%v", fb, err)
	}
	if got.Generation != 1 || got.Arrival != 5000 || got.Watermark != 12345 {
		t.Fatalf("state mismatch: %+v", got)
	}
	if got.Tracks[0].Expected != 100 || len(got.Tracks[0].Missing) != 2 {
		t.Fatalf("track state mismatch: %+v", got.Tracks[0])
	}
	if got.Counters.FirstFault != 42 {
		t.Fatalf("counters mismatch: %+v", got.Counters)
	}
}

func TestTruncatedNewestFallsBack(t *testing.T) {
	dir := t.TempDir()
	st := New(filepath.Join(dir, "ck"))
	if err := st.Save(mkState(1, 5000)); err != nil {
		t.Fatal(err)
	}
	if err := st.Save(mkState(2, 10000)); err != nil {
		t.Fatal(err)
	}
	// Truncate the newest generation (slot 0 for gen 2).
	p := st.slotPath(2)
	fi, _ := os.Stat(p)
	if err := os.Truncate(p, fi.Size()-16); err != nil {
		t.Fatal(err)
	}
	got, fb, err := st.Load()
	if err != nil {
		t.Fatal(err)
	}
	if fb == nil || fb.Kind != media.FaultCheckpointCorrupt {
		t.Fatalf("want checkpoint-corrupt fault, got %v", fb)
	}
	if got.Generation != 1 || got.Arrival != 5000 {
		t.Fatalf("did not fall back to generation 1: %+v", got)
	}
}

func TestBothCorruptIsError(t *testing.T) {
	dir := t.TempDir()
	st := New(filepath.Join(dir, "ck"))
	st.Save(mkState(1, 5000))
	st.Save(mkState(2, 10000))
	for slot := 0; slot < 2; slot++ {
		os.Truncate(st.slotPath(uint64(slot)), 4)
	}
	if _, _, err := st.Load(); err == nil {
		t.Fatal("want error when every generation is corrupt")
	}
}

func TestNewestWinsAcrossSlots(t *testing.T) {
	dir := t.TempDir()
	st := New(filepath.Join(dir, "ck"))
	st.Save(mkState(1, 5000))  // slot 1
	st.Save(mkState(2, 10000)) // slot 0
	st.Save(mkState(3, 15000)) // slot 1 again
	got, _, err := st.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got.Generation != 3 || got.Arrival != 15000 {
		t.Fatalf("got gen %d", got.Generation)
	}
}
