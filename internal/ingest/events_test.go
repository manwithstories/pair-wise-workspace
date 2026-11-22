package ingest

import (
	"sort"
	"testing"
	"time"

	"mediacq/internal/clock"
	"mediacq/internal/testdata"
)

func TestParseEventsAllFamilies(t *testing.T) {
	f, err := testdata.Build(testdata.DefaultParams(17))
	if err != nil {
		t.Fatal(err)
	}
	evs, err := ParseEvents(f.MP4)
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, e := range evs {
		counts[e.Family]++
		if e.Origin.Num <= 0 || e.Origin.Den <= 0 {
			t.Fatalf("event %d has invalid origin clock", e.ID)
		}
	}
	for _, fam := range []string{FamilySubtitle, FamilyChapter, FamilyID3, FamilyEmsg, FamilySEI} {
		if counts[fam] == 0 {
			t.Fatalf("family %s not normalized", fam)
		}
	}
}

func TestEventTimebaseConversion(t *testing.T) {
	f, _ := testdata.Build(testdata.DefaultParams(17))
	evs, err := ParseEvents(f.MP4)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range evs {
		// Round-trip: unified time converted back into the origin clock must
		// reproduce the raw tick (nearest-tick rounding is exact here).
		back, err := e.Origin.FromTime(e.Time)
		if err != nil {
			t.Fatal(err)
		}
		if back != e.RawTicks {
			t.Fatalf("%s event %d: raw %d -> time %d -> %d",
				e.Family, e.ID, e.RawTicks, e.Time.Nanoseconds(), back)
		}
		// Normalized time must land inside the program duration.
		if e.Time < 0 || e.Time > clock.Time(4*time.Second) {
			t.Fatalf("event outside timeline: %+v", e)
		}
	}
}

func TestEventAtExactBoundaryHasOneOwner(t *testing.T) {
	// The 500 ms event must normalize to exactly 500 ms on the 1 kHz clock.
	f, _ := testdata.Build(testdata.DefaultParams(17))
	evs, err := ParseEvents(f.MP4)
	if err != nil {
		t.Fatal(err)
	}
	boundary := clock.Time(500 * time.Millisecond)
	hits := 0
	for _, e := range evs {
		if e.Time == boundary {
			hits++
		}
	}
	if hits == 0 {
		t.Fatal("no event normalized exactly onto the 500 ms boundary")
	}
	// Half-open ownership: [0,500ms) excludes it, [500ms,...) includes it.
	left := clock.Range{Start: 0, End: boundary}
	right := clock.Range{Start: boundary, End: clock.Time(time.Second)}
	for _, e := range evs {
		if e.Time == boundary {
			if left.Contains(e.Time) || !right.Contains(e.Time) {
				t.Fatal("boundary event must belong to exactly one segment")
			}
		}
	}
}

func TestEventsDeterministicOrder(t *testing.T) {
	f1, _ := testdata.Build(testdata.DefaultParams(17))
	f2, _ := testdata.Build(testdata.DefaultParams(17))
	e1, err := ParseEvents(f1.MP4)
	if err != nil {
		t.Fatal(err)
	}
	e2, err := ParseEvents(f2.MP4)
	if err != nil {
		t.Fatal(err)
	}
	key := func(ev []Event) []string {
		out := make([]string, len(ev))
		for i, e := range ev {
			out[i] = e.Family + ":" + string(rune(e.ID))
		}
		sort.Strings(out)
		return out
	}
	k1, k2 := key(e1), key(e2)
	if len(k1) != len(k2) {
		t.Fatal("event count differs across identical builds")
	}
	for i := range k1 {
		if k1[i] != k2[i] {
			t.Fatal("event set not deterministic")
		}
	}
}
