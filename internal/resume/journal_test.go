package resume

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"bcast/internal/fault"
)

func TestJournalRoundTripAndDeterminism(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "journal.json")
	j := &Journal{SchemaVersion: SchemaVersion, SourceSHA256: "a" + repeat("b", 63), SegmentUS: 5_000_000}
	if err := j.Commit(path, Checkpoint{Segment: 0, SampleCount: 10, EndOffset: 256, BlockSHA256: repeat("1", 64)}); err != nil {
		t.Fatal(err)
	}
	if err := j.Commit(path, Checkpoint{Segment: 1, SampleCount: 8, EndOffset: 512, BlockSHA256: repeat("2", 64)}); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.LastCommitted() != 1 || got.SegmentUS != 5_000_000 {
		t.Fatalf("loaded journal wrong: %+v", got)
	}
	if len(got.Committed) != 2 || got.Committed[1].EndOffset != 512 {
		t.Fatalf("checkpoints wrong: %+v", got.Committed)
	}
	// 确定性：重新构建相同状态，序列化字节必须一致。
	b1, _ := os.ReadFile(path)
	j2 := &Journal{SchemaVersion: SchemaVersion, SourceSHA256: j.SourceSHA256, SegmentUS: 5_000_000}
	_ = j2.Commit(filepath.Join(dir, "j2.json"), Checkpoint{0, 10, 256, repeat("1", 64)})
	_ = j2.Commit(filepath.Join(dir, "j2.json"), Checkpoint{1, 8, 512, repeat("2", 64)})
	b2, _ := os.ReadFile(filepath.Join(dir, "j2.json"))
	// 路径不同只影响文件名，不影响内容；比较去除无关后应一致。
	if string(b1) != string(b2) {
		t.Fatal("journal serialization is not deterministic")
	}
}

func TestLoadMissingReturnsSentinel(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "nope.json")); !errors.Is(err, ErrNoJournal) {
		t.Fatalf("want ErrNoJournal, got %v", err)
	}
}

func TestValidationRejectsBrokenJournal(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "j.json")
	cases := []Journal{
		{SchemaVersion: SchemaVersion, Committed: []Checkpoint{{Segment: 1}}}, // 非连续
		{SchemaVersion: SchemaVersion, Committed: []Checkpoint{
			{Segment: 0, EndOffset: 100, BlockSHA256: repeat("1", 64)},
			{Segment: 1, EndOffset: 100, BlockSHA256: repeat("2", 64)}, // 偏移不推进
		}},
		{SchemaVersion: SchemaVersion, Committed: []Checkpoint{{Segment: 0, BlockSHA256: "short"}}},
		{SchemaVersion: 99}, // 版本不符
	}
	for i, j := range cases {
		if err := j.Save(path); err == nil {
			t.Fatalf("case %d must be rejected", i)
		}
	}
}

func TestBindSourceMismatch(t *testing.T) {
	j := &Journal{SchemaVersion: SchemaVersion, SourceSHA256: repeat("a", 64)}
	err := j.BindSource(repeat("c", 64))
	if !errors.Is(err, fault.ErrChecksum) {
		t.Fatalf("want ErrChecksum, got %v", err)
	}
	if err := j.BindSource(repeat("a", 64)); err != nil {
		t.Fatalf("matching source must pass: %v", err)
	}
}

func TestCommitOutOfOrderRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "j.json")
	j := &Journal{SchemaVersion: SchemaVersion, SourceSHA256: repeat("a", 64)}
	// 直接构造一个已有提交的 journal，再尝试乱序提交。
	j.Committed = []Checkpoint{{Segment: 0, EndOffset: 10, BlockSHA256: repeat("1", 64)}}
	err := j.Commit(path, Checkpoint{Segment: 5, EndOffset: 20, BlockSHA256: repeat("2", 64)})
	if !errors.Is(err, fault.ErrBoundary) {
		t.Fatalf("want ErrBoundary, got %v", err)
	}
}

func repeat(s string, n int) string {
	out := make([]byte, n)
	for i := range out {
		out[i] = s[0]
	}
	return string(out)
}
