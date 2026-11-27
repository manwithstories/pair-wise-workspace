package recovery

import (
	"testing"

	"mediad/internal/media"
)

func TestDuplicateClassified(t *testing.T) {
	tr := New(1, 4)
	tr.Observe(10, 0)
	tr.Observe(11, 1)
	c, f := tr.Observe(10, 2)
	if c != ClassDuplicate || f.Kind != media.FaultDuplicate {
		t.Fatalf("class=%v fault=%v", c, f)
	}
	if tr.Stats().Duplicates != 1 {
		t.Fatalf("stats=%+v", tr.Stats())
	}
}

func TestFillableGapAndLateFill(t *testing.T) {
	tr := New(1, 4)
	tr.Observe(10, 0)
	c, f := tr.Observe(13, 1) // gap 11,12
	if c != ClassNew || f.Kind != media.FaultLoss {
		t.Fatalf("class=%v fault=%v", c, f)
	}
	if tr.Stats().Lost != 2 {
		t.Fatalf("lost=%d", tr.Stats().Lost)
	}
	c, f = tr.Observe(11, 2)
	if c != ClassGapFill || f.Kind != media.FaultReorder {
		t.Fatalf("class=%v fault=%v", c, f)
	}
	if tr.Stats().Lost != 1 || tr.Stats().GapFills != 1 {
		t.Fatalf("stats=%+v", tr.Stats())
	}
}

func TestUnrecoverableGap(t *testing.T) {
	tr := New(1, 4)
	tr.Observe(10, 0)
	_, f := tr.Observe(20, 1) // gap of 9 > maxFill 4
	if f.Kind != media.FaultUnrecoverable {
		t.Fatalf("fault=%v", f)
	}
	if tr.Stats().Unrecoverable != 9 {
		t.Fatalf("stats=%+v", tr.Stats())
	}
	// A straggler from an unrecoverable gap is a duplicate, not a fill.
	c, _ := tr.Observe(15, 2)
	if c != ClassDuplicate {
		t.Fatalf("class=%v", c)
	}
}

func TestFinalizeDeclaresLeftover(t *testing.T) {
	tr := New(1, 8)
	tr.Observe(10, 0)
	tr.Observe(13, 1)
	left := tr.Finalize()
	if len(left) != 2 || left[0] != 11 || left[1] != 12 {
		t.Fatalf("leftover=%v", left)
	}
	if tr.Stats().Unrecoverable != 2 || tr.Stats().Lost != 0 {
		t.Fatalf("stats=%+v", tr.Stats())
	}
}

func TestRestoreRoundTrip(t *testing.T) {
	tr := New(1, 8)
	tr.Observe(10, 0)
	tr.Observe(14, 1)
	tr2 := New(1, 8)
	tr2.Restore(tr.Expected(), tr.Missing())
	c, f := tr2.Observe(11, 2)
	if c != ClassGapFill || f.Kind != media.FaultReorder {
		t.Fatalf("after restore: class=%v fault=%v", c, f)
	}
}

func TestSortByMediaTime(t *testing.T) {
	mt := func(n int64) media.MediaTime { return media.MediaTime{Nanos: n, Rate: 48000} }
	items := []TimedItem{
		{Seq: 3, Time: mt(300)},
		{Seq: 1, Time: mt(100)},
		{Seq: 2, Time: mt(200)},
		{Seq: 4, Time: mt(200)}, // tie with seq 2, broken by seq
	}
	SortByMediaTime(items)
	want := []uint64{1, 2, 4, 3}
	for i, w := range want {
		if items[i].Seq != w {
			t.Fatalf("order[%d]=%d want %d", i, items[i].Seq, w)
		}
	}
}
