package pipeline_test

import (
	"errors"
	"path/filepath"
	"testing"

	"tsproof/internal/pipeline"
	"tsproof/internal/segment"
)

func fixtureHashes(t *testing.T, n, per int) []segment.Hash {
	t.Helper()
	pkts := make([][]byte, n*per)
	for i := range pkts {
		p := make([]byte, 188)
		p[0] = 0x47
		p[2] = byte(i)
		pkts[i] = p
	}
	h, err := segment.NewHasher(per)
	if err != nil {
		t.Fatal(err)
	}
	hashes, err := h.HashPackets(pkts)
	if err != nil {
		t.Fatal(err)
	}
	return hashes
}

func TestCommitOnlyCompleteVerifiedSegments(t *testing.T) {
	dir := t.TempDir()
	hashes := fixtureHashes(t, 4, 16)
	store, err := pipeline.NewStore(dir, 20261126, 16)
	if err != nil {
		t.Fatal(err)
	}
	// 提交前两个完整且正确的片段。
	if err := store.CommitSegment(hashes[0], hashes[0]); err != nil {
		t.Fatalf("commit 0: %v", err)
	}
	if err := store.CommitSegment(hashes[1], hashes[1]); err != nil {
		t.Fatalf("commit 1: %v", err)
	}
	if store.CommittedCount() != 2 {
		t.Fatalf("committed=%d want 2", store.CommittedCount())
	}
	// 哈希不匹配的片段必须被拒绝，且不改变已提交数。
	bad := hashes[2]
	bad.SHA256 = "deadbeef"
	if err := store.CommitSegment(bad, hashes[2]); !errors.Is(err, pipeline.ErrCheckpoint) {
		t.Fatalf("corrupt segment must fail, got %v", err)
	}
	if store.CommittedCount() != 2 {
		t.Fatal("rejected commit must not advance checkpoint")
	}
	// 乱序提交（跳过下标 2 直接提交 3）必须拒绝。
	if err := store.CommitSegment(hashes[3], hashes[3]); !errors.Is(err, pipeline.ErrCheckpoint) {
		t.Fatalf("out-of-order commit must fail, got %v", err)
	}
}

func TestResumeSkipsCommittedSegments(t *testing.T) {
	dir := t.TempDir()
	hashes := fixtureHashes(t, 5, 16)
	// 第一次作业：提交前 3 个片段后"中断"。
	first, err := pipeline.NewStore(dir, 77, 16)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := first.CommitSegment(hashes[i], hashes[i]); err != nil {
			t.Fatal(err)
		}
	}
	// 第二次作业：从同一目录恢复，应看到 3 个已提交片段，只重做剩余 2 个。
	second, err := pipeline.NewStore(dir, 77, 16)
	if err != nil {
		t.Fatal(err)
	}
	resumeFrom := second.CommittedCount()
	if resumeFrom != 3 {
		t.Fatalf("resume point=%d want 3", resumeFrom)
	}
	resumed := 0
	for i := resumeFrom; i < len(hashes); i++ {
		if err := second.CommitSegment(hashes[i], hashes[i]); err != nil {
			t.Fatalf("resume commit %d: %v", i, err)
		}
		resumed++
	}
	if resumed != 2 {
		t.Fatalf("resume_segments=%d want 2", resumed)
	}
	if second.CommittedCount() != 5 {
		t.Fatalf("final committed=%d want 5", second.CommittedCount())
	}
}

func TestRejectMismatchedCheckpoint(t *testing.T) {
	dir := t.TempDir()
	first, _ := pipeline.NewStore(dir, 1, 16)
	hashes := fixtureHashes(t, 2, 16)
	if err := first.CommitSegment(hashes[0], hashes[0]); err != nil {
		t.Fatal(err)
	}
	// 不同种子恢复必须拒绝，防止把 A 作业的检查点用到 B 作业。
	if _, err := pipeline.NewStore(dir, 2, 16); !errors.Is(err, pipeline.ErrCheckpoint) {
		t.Fatalf("seed mismatch must fail, got %v", err)
	}
	// 不同分片大小同样拒绝。
	if _, err := pipeline.NewStore(dir, 1, 32); !errors.Is(err, pipeline.ErrCheckpoint) {
		t.Fatalf("segment size mismatch must fail, got %v", err)
	}
}

func TestReplayIdenticalManifest(t *testing.T) {
	dir := t.TempDir()
	hashes := fixtureHashes(t, 3, 16)
	// 两次独立运行落在不同目录，参数完全一致，清单与哈希必须逐项相同。
	dirA := filepath.Join(dir, "a")
	dirB := filepath.Join(dir, "b")
	storeA, _ := pipeline.NewStore(dirA, 909, 16)
	storeB, _ := pipeline.NewStore(dirB, 909, 16)
	for i, h := range hashes {
		line := pipeline.ManifestLine{
			Seq: i, File: "frame", PTS27: int64(i) * 900000,
			Key: i%2 == 0, SHA256: h.SHA256,
		}
		if err := storeA.CommitSegment(h, h); err != nil {
			t.Fatal(err)
		}
		if err := storeA.AppendManifest(line); err != nil {
			t.Fatal(err)
		}
		if err := storeB.CommitSegment(h, h); err != nil {
			t.Fatal(err)
		}
		if err := storeB.AppendManifest(line); err != nil {
			t.Fatal(err)
		}
	}
	ma, mb := storeA.Manifest(), storeB.Manifest()
	if len(ma) != len(mb) {
		t.Fatalf("manifest length differ: %d vs %d", len(ma), len(mb))
	}
	for i := range ma {
		if ma[i].PTS27 != mb[i].PTS27 || ma[i].SHA256 != mb[i].SHA256 ||
			ma[i].Key != mb[i].Key {
			t.Fatalf("replay manifest differs at %d", i)
		}
	}
}
