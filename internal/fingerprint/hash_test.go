package fingerprint

import (
	"testing"

	"archivemedia/internal/model"
)

func sameWindows() []Window {
	return []Window{{ContentHash: 111, Count: 10}, {ContentHash: 222, Count: 8}}
}

func TestSameContentDifferentContainerMerges(t *testing.T) {
	// 同内容、同时间跨度，但来源容器不同（mkv vs mov）：ContentKey 相同 -> 归并
	a := Compute(Input{SegmentID: "a", SourceID: "tape.mkv", Start: 0, End: 1_000_000,
		Tracks: []int{0}, Windows: sameWindows()})
	b := Compute(Input{SegmentID: "b", SourceID: "tape.mov", Start: 0, End: 1_000_000,
		Tracks: []int{0}, Windows: sameWindows()})
	if a.ContentKey != b.ContentKey {
		t.Fatal("同内容 ContentKey 必须一致（跨封装归并）")
	}
	groups := Dedup([]Fingerprint{a, b})
	dups := DuplicateGroups(groups)
	if len(dups) != 1 || len(dups[0].Members) != 2 {
		t.Fatalf("应归并为 1 组 2 成员: %+v", dups)
	}
}

func TestTimeSpanDistinguishesIdenticalContent(t *testing.T) {
	// 内容完全相同但出现在不同时间位置：FullKey 必须不同
	a := Compute(Input{SegmentID: "a", Start: 0, End: 1_000_000,
		Tracks: []int{0}, Windows: sameWindows()})
	b := Compute(Input{SegmentID: "b", Start: 5_000_000, End: 6_000_000,
		Tracks: []int{0}, Windows: sameWindows()})
	if a.FullKey == b.FullKey {
		t.Fatal("不同时间跨度的相同内容 FullKey 必须区分")
	}
	if a.ContentKey != b.ContentKey {
		t.Fatal("内容部分仍应相同")
	}
}

func TestTransitionNotMergedWithPlain(t *testing.T) {
	// 转场段与普通段即使内容哈希相同也不得合并
	plain := Compute(Input{SegmentID: "p", SourceID: "s", Start: 0, End: 1_000_000,
		Tracks: []int{0}, Windows: sameWindows()})
	trans := Compute(Input{SegmentID: "t", SourceID: "s", IsTransition: true,
		Start: 0, End: 1_000_000, Tracks: []int{0, 1}, Windows: sameWindows()})
	groups := Dedup([]Fingerprint{plain, trans})
	for _, g := range groups {
		for _, m := range g.Members {
			if m.Input.IsTransition != g.Members[0].Input.IsTransition {
				t.Fatal("转场段与普通段被误并")
			}
		}
	}
	if len(DuplicateGroups(groups)) != 0 {
		t.Fatal("转场与普通段不应产生重复分组")
	}
}

func TestConflictKeepsCandidates(t *testing.T) {
	// 同内容但来源不同：标记 Conflict 且候选全部保留
	a := Compute(Input{SegmentID: "a", SourceID: "src-A", Start: 0, End: 1_000_000,
		Tracks: []int{0}, Windows: sameWindows()})
	b := Compute(Input{SegmentID: "b", SourceID: "src-B", Start: 0, End: 1_000_000,
		Tracks: []int{0}, Windows: sameWindows()})
	dups := DuplicateGroups(Dedup([]Fingerprint{a, b}))
	if len(dups) != 1 || !dups[0].Conflict {
		t.Fatalf("来源冲突必须标记 Conflict: %+v", dups)
	}
	if len(dups[0].Members) != 2 {
		t.Fatal("冲突候选必须全部保留")
	}
}

func TestDeterministicFingerprint(t *testing.T) {
	in := Input{SegmentID: "x", SourceID: "s", Start: model.MediaTime(123),
		End: model.MediaTime(456), Tracks: []int{2, 0, 1}, Windows: sameWindows()}
	first := Compute(in)
	// 轨道顺序打乱不应影响 FullKey
	in.Tracks = []int{1, 2, 0}
	second := Compute(in)
	if first.FullKey != second.FullKey || first.ContentKey != second.ContentKey {
		t.Fatal("指纹必须对轨道顺序不敏感且可重复")
	}
}

func TestWindowOrderSensitive(t *testing.T) {
	w1 := []Window{{ContentHash: 1, Count: 1}, {ContentHash: 2, Count: 1}}
	w2 := []Window{{ContentHash: 2, Count: 1}, {ContentHash: 1, Count: 1}}
	a := Compute(Input{Windows: w1})
	b := Compute(Input{Windows: w2})
	if a.ContentKey == b.ContentKey {
		t.Fatal("窗序列顺序必须敏感，防止乱序内容误判相同")
	}
}
