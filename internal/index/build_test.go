package index

import (
	"testing"

	mp4scan "mediaqa/internal/mp4"
)

func samples() []mp4scan.Sample {
	// Sparse keyframes at 0 and 300; sample 2 corrupt.
	return []mp4scan.Sample{
		{Index: 0, DTS: 0, PTS: 0, Duration: 100, Key: true, Offset: 1000, Size: 50},
		{Index: 1, DTS: 100, PTS: 100, Duration: 100, Offset: 1050, Size: 40},
		{Index: 2, DTS: 200, PTS: 200, Duration: 100, Corrupt: true},
		{Index: 3, DTS: 300, PTS: 300, Duration: 100, Key: true, Offset: 1090, Size: 60},
		{Index: 4, DTS: 400, PTS: 400, Duration: 100, Offset: 1150, Size: 40},
	}
}

func identity(t int64) (int64, bool) { return t, true }

func TestSeekFallsBackToPreviousKeyframe(t *testing.T) {
	ix := Build("v1", 90000, samples(), identity)
	e, ok := ix.Seek(450)
	if !ok || e.PresTicks != 300 {
		t.Fatalf("seek 450 -> %+v ok=%v", e, ok)
	}
	e, ok = ix.Seek(250) // corrupt sample at 200 must be skipped
	if !ok || e.PresTicks != 0 {
		t.Fatalf("seek 250 -> %+v ok=%v", e, ok)
	}
	if _, ok := ix.Seek(-1); ok {
		t.Fatal("seek before start must fail")
	}
	e, ok = ix.Seek(400)
	if !ok || e.PresTicks != 300 {
		t.Fatalf("seek 400 -> %+v ok=%v", e, ok)
	}
}

func TestMarshalRoundTripDeterministic(t *testing.T) {
	ix := Build("v1", 90000, samples(), identity)
	a := ix.Marshal()
	b := Build("v1", 90000, samples(), identity).Marshal()
	if string(a) != string(b) {
		t.Fatal("marshal must be deterministic")
	}
	back, err := Unmarshal(a)
	if err != nil {
		t.Fatal(err)
	}
	if back.TrackID != "v1" || back.Timescale != 90000 || len(back.Entries) != 4 {
		t.Fatalf("round trip: %+v", back)
	}
	if !back.Entries[0].Key || back.Entries[3].PresTicks != 400 {
		t.Fatalf("entries: %+v", back.Entries)
	}
}

func TestEditMappingGapsExcluded(t *testing.T) {
	// Presentation mapping with a gap: ticks >= 1000 unmapped.
	presOf := func(dts int64) (int64, bool) {
		if dts >= 300 {
			return 0, false
		}
		return dts, true
	}
	ix := Build("v1", 90000, samples(), presOf)
	if len(ix.Entries) != 2 { // corrupt + unmapped excluded
		t.Fatalf("entries=%d", len(ix.Entries))
	}
}
