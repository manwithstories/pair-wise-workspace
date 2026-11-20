package align

import "testing"

func rungFrom(ticks ...int64) Rung {
	r := Rung{ID: "r"}
	for _, t := range ticks {
		r.Cuts = append(r.Cuts, Cut{Ticks: t, Keyframe: true})
	}
	return r
}

func TestAlignWithinTolerance(t *testing.T) {
	canonical := []int64{0, 90000, 180000}
	rungs := []Rung{rungFrom(0, 90000, 180000), rungFrom(0, 90900, 179100)}
	res := Align(canonical, rungs, 1800, 7200) // tol 20ms, fallback 80ms
	if len(res.Gaps) != 0 {
		t.Fatalf("gaps: %+v", res.Gaps)
	}
	if res.ErrorMs < 9.9 || res.ErrorMs > 10.1 {
		t.Fatalf("error: %v", res.ErrorMs)
	}
}

func TestAlignKeyframeFallback(t *testing.T) {
	canonical := []int64{0, 90000}
	// Rung places its cut 40 ms past canonical: outside 20 ms tolerance,
	// but a keyframe sits within the 80 ms fallback.
	rungs := []Rung{rungFrom(0, 90000), rungFrom(0, 93600)}
	res := Align(canonical, rungs, 1800, 7200)
	if len(res.Gaps) != 0 {
		t.Fatalf("fallback should avoid gaps: %+v", res.Gaps)
	}
	if res.ErrorMs < 39.9 || res.ErrorMs > 40.1 {
		t.Fatalf("error: %v", res.ErrorMs)
	}
}

func TestAlignGapNeverExtendsNeighbors(t *testing.T) {
	canonical := []int64{0, 90000, 180000, 270000}
	// Rung is missing the cut at 90000 entirely; next cut is far away.
	rungs := []Rung{
		rungFrom(0, 90000, 180000, 270000),
		rungFrom(0, 180000, 270000),
	}
	res := Align(canonical, rungs, 1800, 7200)
	if len(res.Gaps) != 1 {
		t.Fatalf("want exactly one gap: %+v", res.Gaps)
	}
	g := res.Gaps[0]
	if g.Start != 90000 || g.End != 180000 {
		t.Fatalf("gap interval must match the canonical interval: %+v", g)
	}
	// The canonical cuts stay intact: no silent extension.
	if len(res.Cuts) != 4 || res.Cuts[1] != 90000 {
		t.Fatalf("canonical cuts modified: %+v", res.Cuts)
	}
}

func TestAlignEmptyRung(t *testing.T) {
	canonical := []int64{0, 90000}
	res := Align(canonical, []Rung{rungFrom(0, 90000), {ID: "empty"}}, 1800, 7200)
	if len(res.Gaps) != 2 {
		t.Fatalf("empty rung must gap every interval: %+v", res.Gaps)
	}
}
