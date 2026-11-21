package ladder

import (
	"testing"

	"archivecheck/internal/model"
)

func manifest(rep string, starts []int64, dur int64) model.Manifest {
	m := model.Manifest{Rep: rep}
	for i, s := range starts {
		m.Segments = append(m.Segments, model.Segment{
			Index: i, Rep: rep, FirstSeq: uint32(i * 60),
			StartNS: s, EndNS: s + dur,
		})
	}
	return m
}

func TestFaninAlignedAccepted(t *testing.T) {
	r1 := RepResult{Rep: "360p", Manifest: manifest("360p", []int64{0, 2_000_000_000, 4_000_000_000}, 1_999_999_990)}
	r2 := RepResult{Rep: "720p", Manifest: manifest("720p", []int64{0, 2_000_000_050, 4_000_000_050}, 1_999_999_990)}
	report := Fanin([]RepResult{r1, r2}, 100_000_000, func(string, uint32) bool { return false })
	if !report.Accepted || len(report.Mismatches) != 0 {
		t.Fatalf("对齐清单应通过: %+v", report)
	}
}

func TestFaninBoundaryConflictRejected(t *testing.T) {
	r1 := RepResult{Rep: "360p", Manifest: manifest("360p", []int64{0, 2_000_000_000}, 2_000_000_000)}
	// 720p 第二个边界偏离 300ms，超过 100ms 容差。
	r2 := RepResult{Rep: "720p", Manifest: manifest("720p", []int64{0, 2_300_000_000}, 2_000_000_000)}
	report := Fanin([]RepResult{r1, r2}, 100_000_000, func(string, uint32) bool { return false })
	if report.Accepted {
		t.Fatal("超差边界必须拒绝发布")
	}
	if len(report.Mismatches) != 1 || report.Mismatches[0].Index != 1 {
		t.Fatalf("应定位到第 1 号边界冲突: %+v", report.Mismatches)
	}
}

func TestFaninSegmentCountMismatchRejected(t *testing.T) {
	r1 := RepResult{Rep: "360p", Manifest: manifest("360p", []int64{0, 2_000_000_000}, 2_000_000_000)}
	r2 := RepResult{Rep: "720p", Manifest: manifest("720p", []int64{0}, 2_000_000_000)}
	report := Fanin([]RepResult{r1, r2}, 100_000_000, func(string, uint32) bool { return false })
	if report.Accepted {
		t.Fatal("分片数不一致必须拒绝")
	}
}

func TestFaninIsolatedBoundaryRejected(t *testing.T) {
	r1 := RepResult{Rep: "360p", Manifest: manifest("360p", []int64{0, 2_000_000_000}, 2_000_000_000)}
	r2 := RepResult{Rep: "720p", Manifest: manifest("720p", []int64{0, 2_000_000_000}, 2_000_000_000)}
	isolated := func(rep string, seq uint32) bool { return rep == "720p" && seq == 60 }
	report := Fanin([]RepResult{r1, r2}, 100_000_000, isolated)
	if report.Accepted {
		t.Fatal("隔离时钟段上的边界必须拒绝发布")
	}
}

func TestFaninLayerErrorRejected(t *testing.T) {
	r1 := RepResult{Rep: "360p", Manifest: manifest("360p", []int64{0}, 2_000_000_000)}
	r2 := RepResult{Rep: "720p", FirstErr: &RepError{Seq: 10, Fault: model.FaultHashMismatch}}
	report := Fanin([]RepResult{r1, r2}, 100_000_000, func(string, uint32) bool { return false })
	if report.Accepted {
		t.Fatal("携带首错的表示层必须导致拒绝发布")
	}
}

func TestFanoutDeterministicOrder(t *testing.T) {
	frames := map[string][]model.Frame{
		"720p": {{Rep: "720p", Seq: 0}},
		"360p": {{Rep: "360p", Seq: 0}},
	}
	var order []string
	results := Fanout(frames, func(rep string, fs []model.Frame) RepResult {
		order = append(order, rep)
		return RepResult{Rep: rep}
	})
	if order[0] != "360p" || order[1] != "720p" {
		t.Fatalf("扇出顺序不确定: %v", order)
	}
	if results[0].Rep != "360p" {
		t.Fatal("结果顺序应与字典序一致")
	}
}
