package index

import (
	"testing"

	"mediarepair/internal/model"
)

func rs(track uint32, seq uint64, dts uint64, key bool, seg string) model.RepairedSample {
	return model.RepairedSample{
		Sample:      model.Sample{TrackID: track, Seq: seq, Keyframe: key, SourceOffset: int64(seq) * 100},
		UnifiedDTS:  dts,
		SegmentID:   seg,
	}
}

func TestBuildAndSeek(t *testing.T) {
	ix := New()
	for i := 0; i < 10; i++ {
		if err := ix.AddSample(rs(1, uint64(i), uint64(i)*1000, i%3 == 0, "s")); err != nil {
			t.Fatal(err)
		}
	}
	ix.Finalize()
	if ix.EntryCount() != 4 {
		t.Fatalf("关键帧条目数 %d", ix.EntryCount())
	}
	e, ok := ix.Seek(1, 3500)
	if !ok || e.Seq != 3 {
		t.Fatalf("Seek(3500)=%+v ok=%v，期望序号 3", e, ok)
	}
	if _, ok := ix.Seek(1, 0); ok {
		// 时间 0 处有关键帧，应命中。
	} else {
		t.Fatal("时间 0 应命中关键帧")
	}
	if _, ok := ix.Seek(2, 1000); ok {
		t.Fatal("轨道 2 无条目不应命中")
	}
	seeks, hits := ix.Stats()
	if seeks != 3 || hits != 2 {
		t.Fatalf("seeks=%d hits=%d", seeks, hits)
	}
}

func TestRejectsMissingSample(t *testing.T) {
	ix := New()
	err := ix.AddEntry(Entry{TrackID: 1, Seq: 7, UnifiedDTS: 0})
	if err == nil {
		t.Fatal("指向未注册样本的条目应被拒绝")
	}
	if ix.EntryCount() != 0 {
		t.Fatal("被拒绝的条目不应进入索引")
	}
}

func TestAddSampleRegistersBeforeKeyCheck(t *testing.T) {
	ix := New()
	if err := ix.AddSample(rs(1, 0, 0, true, "s")); err != nil {
		t.Fatal(err)
	}
	ix.Finalize()
	if ix.EntryCount() != 1 {
		t.Fatalf("首个关键帧应被索引: %d", ix.EntryCount())
	}
}

func TestHitRateNoSeeks(t *testing.T) {
	ix := New()
	if ix.HitRate() != 1 {
		t.Fatalf("无查找时命中率应为 1，实际 %v", ix.HitRate())
	}
}
