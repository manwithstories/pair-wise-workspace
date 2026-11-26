package manifest

import (
	"testing"

	"archivemedia/internal/journal"
)

func rec(id, key, status string) *journal.SegmentRecord {
	return &journal.SegmentRecord{
		SegmentID: id, SourceID: "src", IdempotencyKey: key,
		AdapterName: "arc1", AdapterVersion: "1.0.0",
		FilePath: "segments/" + id + ".arc", FileSha256: "h-" + id,
		StartUs: 0, EndUs: 1000, Status: journal.Status(status),
		Provenance: "prov-" + id, Params: map[string]string{"k": "v"},
	}
}

func TestBuildOnlyCommittedAndSorted(t *testing.T) {
	// 调用方只传已提交记录；验证乱序输入被按幂等键排序
	records := []*journal.SegmentRecord{
		rec("seg-2", "key-z", "committed"),
		rec("seg-1", "key-a", "committed"),
	}
	m := Build(records, "run-1", "2026-01-01T00:00:00Z", RunStats{SilenceIntervals: 2, DuplicateGroups: 1})
	if m.SegmentCount != 2 {
		t.Fatalf("count=%d", m.SegmentCount)
	}
	if m.Segments[0].IdempotencyKey != "key-a" || m.Segments[1].IdempotencyKey != "key-z" {
		t.Fatal("清单必须按幂等键确定性排序")
	}
	if m.Segments[0].ProvenanceSha256 != "prov-seg-1" {
		t.Fatal("溯源字段必须透传")
	}
}

func TestAtomicWriteReadAndStableBytes(t *testing.T) {
	root := t.TempDir()
	m := Build([]*journal.SegmentRecord{rec("seg-1", "key-a", "committed")},
		"run-1", "fixed-time", RunStats{})
	h1, err := WriteAtomically(root, m)
	if err != nil {
		t.Fatal(err)
	}
	back, err := Read(root)
	if err != nil {
		t.Fatal(err)
	}
	if back.SegmentCount != 1 || back.Segments[0].FileSha256 != "h-seg-1" {
		t.Fatalf("读回清单不一致: %+v", back)
	}
	// 重复写出相同内容必须字节一致、哈希一致
	h2, err := WriteAtomically(root, m)
	if err != nil || h1 != h2 {
		t.Fatalf("重复运行清单哈希必须一致: %s vs %s err=%v", h1, h2, err)
	}
	// 内容哈希与写盘哈希一致（序列化不含自身哈希字段）
	contentHash, err := ContentSha256(m)
	if err != nil || contentHash != h1 {
		t.Fatalf("内容哈希=%s 写盘哈希=%s", contentHash, h1)
	}
}

func TestManifestSchemaAndEmpty(t *testing.T) {
	m := Build(nil, "run-empty", "t", RunStats{})
	if m.SchemaVersion != "1.0" || m.SegmentCount != 0 {
		t.Fatalf("空清单字段错误: %+v", m)
	}
	data, err := marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if data[len(data)-1] != '\n' {
		t.Fatal("清单应以换行结尾，保证 POSIX 文本一致")
	}
}
