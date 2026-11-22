package index

import (
	"testing"

	"mediad/internal/model"
)

func samples(n int, keyEvery uint32, incomplete ...uint32) []model.Sample {
	out := make([]model.Sample, n)
	bad := map[uint32]bool{}
	for _, b := range incomplete {
		bad[b] = true
	}
	for i := 0; i < n; i++ {
		idx := uint32(i + 1)
		out[i] = model.Sample{
			TrackID: "trk-1", Index: idx, PTS: int64(i) * 100,
			Duration: 100, Complete: !bad[idx],
			Keyframe: keyEvery > 0 && (uint32(i)%keyEvery == 0),
		}
	}
	return out
}

func TestRebuildFromCompleteSamples(t *testing.T) {
	s := samples(12, 3)
	idx := Rebuild("trk-1", s, false)
	if !idx.Rebuilt || !idx.Complete {
		t.Fatalf("rebuilt=%v complete=%v", idx.Rebuilt, idx.Complete)
	}
	want := []uint32{1, 4, 7, 10}
	if len(idx.Keyframes) != len(want) {
		t.Fatalf("keyframes = %v, want %v", idx.Keyframes, want)
	}
	for i, w := range want {
		if idx.Keyframes[i] != w || !idx.HasKeyframe(w) {
			t.Fatalf("keyframe mismatch at %d", i)
		}
	}
	if idx.HasKeyframe(2) {
		t.Fatal("non-keyframe must not be reported")
	}
}

func TestRebuildMidGapNoFabrication(t *testing.T) {
	// 样本 5、6 损坏：区间 [400,600) 不可信，其余关键帧照常重建。
	s := samples(12, 3, 5, 6)
	idx := Rebuild("trk-1", s, false)
	if idx.Complete {
		t.Fatal("incomplete samples must clear Complete")
	}
	if len(idx.IncompleteRanges) != 1 || idx.IncompleteRanges[0] != [2]int64{400, 600} {
		t.Fatalf("gap range wrong: %+v", idx.IncompleteRanges)
	}
	// 样本 7 是关键帧且完整，必须保留；损坏区间内无条目。
	if !idx.HasKeyframe(7) || idx.HasKeyframe(5) || idx.HasKeyframe(6) {
		t.Fatalf("gap handling wrong: %v", idx.Keyframes)
	}
}

func TestRebuildTailTruncationNoFakeTailKeyframe(t *testing.T) {
	// 最后三个样本截断：索引必须在截断点终止，绝不补尾部关键帧。
	s := samples(12, 3, 10, 11, 12)
	idx := Rebuild("trk-1", s, false)
	if idx.Complete {
		t.Fatal("tail truncation must clear Complete")
	}
	for _, k := range idx.Keyframes {
		if k >= 10 {
			t.Fatalf("fabricated keyframe %d inside truncated tail", k)
		}
	}
	last := idx.IncompleteRanges[len(idx.IncompleteRanges)-1]
	if last != [2]int64{900, 1200} {
		t.Fatalf("tail gap = %v, want [900,1200)", last)
	}
}

func TestVerifyDetectsMismatch(t *testing.T) {
	s := samples(12, 3)
	idx := Rebuild("trk-1", s, false)
	if errs := Verify(idx, []uint32{1, 4, 7, 10}); len(errs) != 0 {
		t.Fatalf("exact match must verify clean: %v", errs)
	}
	// 99 为虚构条目，7/10 缺失：共 3 处不符。
	errs := Verify(idx, []uint32{1, 4, 99})
	if len(errs) != 3 {
		t.Fatalf("want 3 mismatch faults, got %d: %v", len(errs), errs)
	}
}

func TestHitRate(t *testing.T) {
	s := samples(10, 2) // 关键帧 1,3,5,7,9
	idx := Rebuild("trk-1", s, true)
	hits, total := HitRate(idx, s)
	if hits != 5 || total != 5 {
		t.Fatalf("hit rate = %d/%d", hits, total)
	}
	// 截断尾部后命中率下降但不虚构。
	s2 := samples(10, 2, 9)
	idx2 := Rebuild("trk-1", s2, true)
	hits2, total2 := HitRate(idx2, s2)
	if total2 != 5 || hits2 != 4 {
		t.Fatalf("truncated hit rate = %d/%d, want 4/5", hits2, total2)
	}
}
