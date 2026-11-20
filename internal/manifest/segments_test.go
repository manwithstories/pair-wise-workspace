package manifest

import (
	"path/filepath"
	"testing"

	"media-audit/internal/stream"
)

func sampleInput() BuildInput {
	return BuildInput{
		Codec:  "av1",
		RepIDs: []string{"r0", "r1", "r2"},
		CutPoints: []CutPoint{
			{
				AnchorMicros: 0,
				EndMicros:    10_000_000,
				Ranges: map[string]ByteRange{
					"r0": {0, 1000}, "r1": {0, 900}, "r2": {0, 1100},
				},
			},
			{
				AnchorMicros: 10_000_000,
				EndMicros:    20_000_000,
				Ranges: map[string]ByteRange{
					"r0": {1000, 2000}, "r1": {900, 1800}, "r2": {1100, 2200},
				},
			},
		},
		Issues: []stream.Issue{
			{TrackID: 0, Offset: 1500, End: 1510, Block: 1, Kind: stream.IssueForbiddenNAL},
			{TrackID: 1, Offset: 500, End: 510, Block: 0, Kind: stream.IssueIllegalOBU},
		},
	}
}

func TestBuildStableAndDigest(t *testing.T) {
	m1 := Build(sampleInput())
	m2 := Build(sampleInput())
	if m1.SegmentCount != 2 {
		t.Fatalf("分片数=%d", m1.SegmentCount)
	}
	if m1.SHA256 != m2.SHA256 {
		t.Fatalf("相同输入两次构建的清单校验和不一致")
	}
	for i := range m1.Segments {
		if m1.Segments[i].SHA256 != m2.Segments[i].SHA256 {
			t.Fatalf("分片 %d 校验和不稳定", i)
		}
		if len(m1.Segments[i].SHA256) != 64 {
			t.Fatalf("分片 SHA256 长度错误")
		}
	}
	// 错误按字节偏移归入正确分片。
	if len(m1.Segments[0].Errors) != 1 || m1.Segments[0].Errors[0].Offset != 500 {
		t.Fatalf("分片0 错误归并错误: %+v", m1.Segments[0].Errors)
	}
	if len(m1.Segments[1].Errors) != 1 {
		t.Fatalf("分片1 应含 1 条错误")
	}
	if m1.ErrorKinds[string(stream.IssueForbiddenNAL)] != 1 {
		t.Fatalf("错误类别统计错误: %+v", m1.ErrorKinds)
	}
}

func TestSegmentDurationAndOrder(t *testing.T) {
	m := Build(sampleInput())
	for i, s := range m.Segments {
		if s.DurationUS != s.EndMicros-s.StartMicros {
			t.Fatalf("分片 %d 时长计算错误", i)
		}
		if s.Index != i {
			t.Fatalf("分片序号乱序")
		}
	}
}

func TestWriteAtomicRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "manifest.json")
	m := Build(sampleInput())
	if err := WriteAtomic(path, m); err != nil {
		t.Fatal(err)
	}
	got, err := ReadAtomic(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.SHA256 != m.SHA256 || got.SegmentCount != 2 {
		t.Fatalf("原子写回读不一致")
	}
	// 临时文件不应残留。
	matches, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".manifest-*.tmp"))
	if len(matches) != 0 {
		t.Fatalf("临时文件未清理: %v", matches)
	}
}

func TestDigestChangesWithContent(t *testing.T) {
	m1 := Build(sampleInput())
	in := sampleInput()
	in.CutPoints[0].EndMicros = 11_000_000
	m2 := Build(in)
	if m1.SHA256 == m2.SHA256 {
		t.Fatalf("内容变化后校验和必须改变")
	}
}
