package index

import (
	"testing"

	"media-audit/internal/stream"
)

// 构造确定性扫描结果：6 个 AU，关键帧在 0、3，块大小 3。
func fakeScan() *stream.ScanResult {
	return &stream.ScanResult{
		Codec:   stream.H264,
		TrackID: 0,
		Size:    60,
		AUs: []stream.AccessUnit{
			{Index: 0, Offset: 0, End: 10, Block: 0, Keyframe: true},
			{Index: 1, Offset: 10, End: 20, Block: 0},
			{Index: 2, Offset: 20, End: 30, Block: 0},
			{Index: 3, Offset: 30, End: 40, Block: 1, Keyframe: true},
			{Index: 4, Offset: 40, End: 50, Block: 1},
			{Index: 5, Offset: 50, End: 60, Block: 1},
		},
		Blocks: []stream.BlockRecord{
			{Number: 0, Start: 0, End: 30, SHA256: "aa"},
			{Number: 1, Start: 30, End: 60, SHA256: "bb"},
		},
	}
}

func TestMissingIndexRebuilt(t *testing.T) {
	res, err := Verify(nil, fakeScan(), "hash1")
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusMissing {
		t.Fatalf("状态=%s, 期望 rebuilt(missing)", res.Status)
	}
	if len(res.Rebuilt.Entries) != 2 || res.Rebuilt.Entries[1].Offset != 30 {
		t.Fatalf("重建索引错误: %+v", res.Rebuilt.Entries)
	}
}

func TestStaleIndexDetectedByHash(t *testing.T) {
	ext := &Index{TrackID: 0, StreamHash: "old-hash", Entries: []Entry{{0, 0}}}
	res, err := Verify(ext, fakeScan(), "new-hash")
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusRebuilt || len(res.Problems) != 1 ||
		res.Problems[0].Kind != KindStale {
		t.Fatalf("过期索引判定错误: %+v", res)
	}
}

func TestDuplicateEntry(t *testing.T) {
	ext := &Index{
		TrackID: 0, StreamHash: "h",
		Entries: []Entry{{0, 0}, {0, 0}, {3, 30}},
	}
	res, err := Verify(ext, fakeScan(), "h")
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusSuspicious {
		t.Fatalf("状态=%s, 期望 suspicious", res.Status)
	}
	if res.Problems[0].Kind != KindDuplicate {
		t.Fatalf("未检出重复条目: %+v", res.Problems)
	}
}

func TestOutOfRangeEntry(t *testing.T) {
	ext := &Index{
		TrackID: 0, StreamHash: "h",
		Entries: []Entry{{0, 0}, {9, 9999}},
	}
	res, _ := Verify(ext, fakeScan(), "h")
	var found bool
	for _, p := range res.Problems {
		if p.Kind == KindOutOfRange {
			found = true
			if p.RangeLo < 0 || p.RangeHi > 60 {
				t.Fatalf("可疑范围未夹取到流内: %+v", p)
			}
		}
	}
	if !found {
		t.Fatalf("未检出越界条目")
	}
}

func TestMismatchReportsMinimalAURange(t *testing.T) {
	// 偏移 15 落在 AU1 [10,20) 内但不是关键帧。
	ext := &Index{
		TrackID: 0, StreamHash: "h",
		Entries: []Entry{{0, 0}, {3, 15}},
	}
	res, _ := Verify(ext, fakeScan(), "h")
	var p *Problem
	for i := range res.Problems {
		if res.Problems[i].Kind == KindMismatch {
			p = &res.Problems[i]
		}
	}
	if p == nil {
		t.Fatalf("未检出关键帧不匹配")
	}
	if p.RangeLo != 10 || p.RangeHi != 20 {
		t.Fatalf("最小可疑范围=[%d,%d], 期望 [10,20]", p.RangeLo, p.RangeHi)
	}
}

func TestValidIndexAccepted(t *testing.T) {
	ext := &Index{
		TrackID: 0, StreamHash: "h",
		Entries: []Entry{{0, 0}, {3, 30}},
	}
	res, err := Verify(ext, fakeScan(), "h")
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusValid {
		t.Fatalf("合法索引被误判: %+v", res)
	}
}

func TestHashBytesDeterministic(t *testing.T) {
	if HashBytes([]byte("x")) == HashBytes([]byte("y")) {
		t.Fatalf("不同内容哈希不应相同")
	}
	if HashBytes([]byte("x")) != HashBytes([]byte("x")) {
		t.Fatalf("相同内容哈希应稳定")
	}
}
