package process

import "testing"

func TestSplitExactBoundary(t *testing.T) {
	segs, err := Split(0, 3000, 1000, 400)
	if err != nil {
		t.Fatal(err)
	}
	if len(segs) != 3 {
		t.Fatalf("segs=%d", len(segs))
	}
	if segs[2].Reason != "boundary_exact" || segs[2].End != 3000 {
		t.Fatalf("last=%+v", segs[2])
	}
}

func TestSplitBelowMin(t *testing.T) {
	segs, err := Split(100, 200, 1000, 400)
	if err != nil {
		t.Fatal(err)
	}
	if len(segs) != 1 || segs[0].Reason != "below_min_window" {
		t.Fatalf("segs=%+v", segs)
	}
	if segs[0].Start != 100 || segs[0].End != 200 {
		t.Fatalf("bounds=%+v", segs[0])
	}
}

func TestSplitTailMerged(t *testing.T) {
	// 3200 = 3*1000 + 200，余量 200 < min 400 → 并入前段
	segs, err := Split(0, 3200, 1000, 400)
	if err != nil {
		t.Fatal(err)
	}
	if len(segs) != 3 {
		t.Fatalf("segs=%d", len(segs))
	}
	last := segs[2]
	if last.Reason != "tail_merged" || last.Start != 2000 || last.End != 3200 {
		t.Fatalf("last=%+v", last)
	}
}

func TestSplitTailPartial(t *testing.T) {
	// 3500 = 3*1000 + 500，余量 500 >= min 400 → 独立尾段
	segs, err := Split(0, 3500, 1000, 400)
	if err != nil {
		t.Fatal(err)
	}
	if len(segs) != 4 || segs[3].Reason != "tail_partial" {
		t.Fatalf("segs=%+v", segs)
	}
	if segs[3].Start != 3000 || segs[3].End != 3500 {
		t.Fatalf("tail=%+v", segs[3])
	}
}

func TestSplitInvalid(t *testing.T) {
	if _, err := Split(0, 100, 0, 10); err == nil {
		t.Fatal("expected error")
	}
	if _, err := Split(100, 100, 10, 1); err == nil {
		t.Fatal("expected error")
	}
}
