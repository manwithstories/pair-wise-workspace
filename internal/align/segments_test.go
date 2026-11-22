package align

import "testing"

func keyframesEvery(step, total int64) []int64 {
	var ks []int64
	for f := int64(0); f < total; f += step {
		ks = append(ks, f)
	}
	return ks
}

func TestCutBoundariesAndCount(t *testing.T) {
	total := int64(17982) // 10 min at 30000/1001
	segs := Cut(total, 180, keyframesEvery(60, total), nil, nil)
	if len(segs) != 100 {
		t.Fatalf("segments = %d, want 100", len(segs))
	}
	for i, s := range segs {
		if s.Index != i || !s.RandomAccess {
			t.Fatalf("segment %+v", s)
		}
		if i > 0 && s.StartFrame != segs[i-1].EndFrame {
			t.Fatalf("non-contiguous at %d", i)
		}
		if s.StartFrame%60 != 0 {
			t.Fatalf("boundary %d not on keyframe", s.StartFrame)
		}
	}
	if segs[len(segs)-1].EndFrame != total {
		t.Fatalf("last end = %d", segs[len(segs)-1].EndFrame)
	}
}

func TestCutLostKeyframeNotRandomAccess(t *testing.T) {
	total := int64(600)
	lost := map[int64]bool{360: true}
	segs := Cut(total, 180, keyframesEvery(60, total), lost, nil)
	if segs[2].StartFrame != 360 || segs[2].RandomAccess {
		t.Fatalf("segment 2 = %+v, want non-random-access at 360", segs[2])
	}
	if !segs[0].RandomAccess || !segs[1].RandomAccess {
		t.Fatal("intact keyframes must stay random-access")
	}
}

func TestCutGapsAssigned(t *testing.T) {
	total := int64(600)
	gaps := [][2]int64{{100, 120}, {590, 600}}
	segs := Cut(total, 180, keyframesEvery(60, total), nil, gaps)
	if len(segs[0].Gaps) != 1 || segs[0].Gaps[0] != [2]int64{100, 120} {
		t.Fatalf("seg0 gaps = %v", segs[0].Gaps)
	}
	last := segs[len(segs)-1]
	if len(last.Gaps) != 1 || last.Gaps[0] != [2]int64{590, 600} {
		t.Fatalf("last gaps = %v", last.Gaps)
	}
	if len(segs[1].Gaps) != 0 {
		t.Fatalf("seg1 gaps = %v", segs[1].Gaps)
	}
}

func TestCutSnapsToNearestKeyframe(t *testing.T) {
	// Keyframes at 0, 100, 210; target 100 -> boundaries snap to 0,100,210.
	segs := Cut(300, 100, []int64{0, 100, 210}, nil, nil)
	if len(segs) != 3 || segs[1].StartFrame != 100 || segs[2].StartFrame != 210 {
		t.Fatalf("segs = %+v", segs)
	}
}
