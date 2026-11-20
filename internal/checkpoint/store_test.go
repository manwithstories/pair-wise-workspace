package checkpoint

import (
	"path/filepath"
	"testing"

	"media-audit/internal/stream"
)

func sampleStore(job string) *Store {
	return &Store{
		JobID: job,
		Tracks: []TrackState{
			{
				TrackID: 0, Codec: "av1", InputSize: 1000, InputSHA256: "hash0",
				Completed: []stream.BlockRecord{
					{Number: 0, Start: 0, End: 500, SHA256: "b0"},
					{Number: 1, Start: 500, End: 1000, SHA256: "b1"},
				},
			},
		},
		OutputSHA256: "out-hash",
	}
}

func expectedRefs() map[int]InputRef {
	return map[int]InputRef{0: {Size: 1000, SHA256: "hash0"}}
}

func TestSaveLoadRoundTripReusable(t *testing.T) {
	dir := t.TempDir()
	if err := Save(dir, sampleStore("job1")); err != nil {
		t.Fatal(err)
	}
	res, err := Load(dir, "job1", expectedRefs())
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != OutcomeReusable {
		t.Fatalf("判定=%s, 期望 reusable", res.Outcome)
	}
	if len(res.Reusable[0]) != 2 {
		t.Fatalf("可复用块数=%d", len(res.Reusable[0]))
	}
}

func TestLoadMissingTriggersReset(t *testing.T) {
	res, err := Load(t.TempDir(), "nope", expectedRefs())
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != OutcomeReset || res.ResetReason == "" {
		t.Fatalf("缺失检查点应回退: %+v", res)
	}
}

func TestVersionMismatchResets(t *testing.T) {
	dir := t.TempDir()
	st := sampleStore("job2")
	if err := Save(dir, st); err != nil {
		t.Fatal(err)
	}
	// 篡改磁盘上的版本号。
	p := filepath.Join(dir, "checkpoint-job2.json")
	raw := readFile(t, p)
	raw = replaceFirst(raw, `"schema_version": 1`, `"schema_version": 99`)
	writeFile(t, p, raw)
	res, _ := Load(dir, "job2", expectedRefs())
	if res.Outcome != OutcomeReset {
		t.Fatalf("版本不匹配必须整体回退, got %s", res.Outcome)
	}
}

func TestInputDigestMismatchPartial(t *testing.T) {
	dir := t.TempDir()
	if err := Save(dir, sampleStore("job3")); err != nil {
		t.Fatal(err)
	}
	changed := map[int]InputRef{0: {Size: 2000, SHA256: "different"}}
	res, _ := Load(dir, "job3", changed)
	if res.Outcome != OutcomePartial {
		t.Fatalf("输入摘要变化应 partial, got %s", res.Outcome)
	}
	if len(res.Reusable[0]) != 0 {
		t.Fatalf("变化轨道的块必须全部丢弃")
	}
}

func TestVerifyBlocksDropsCorrupt(t *testing.T) {
	blocks := []stream.BlockRecord{
		{Number: 0, SHA256: "good0"},
		{Number: 1, SHA256: "good1"},
		{Number: 2, SHA256: "stale2"},
	}
	hashes := func(n int) (string, error) {
		if n == 2 {
			return "actual2", nil // 磁盘内容已变
		}
		return blocks[n].SHA256, nil
	}
	good := VerifyBlocks(blocks, hashes)
	if len(good) != 2 || good[1].Number != 1 {
		t.Fatalf("损坏块未被剔除: %+v", good)
	}
}

func TestTamperedCheckpointSelfHashFails(t *testing.T) {
	dir := t.TempDir()
	if err := Save(dir, sampleStore("job4")); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "checkpoint-job4.json")
	raw := readFile(t, p)
	// 篡改输出哈希但保留自校验字段，模拟外部破坏。
	raw = replaceFirst(raw, `"output_sha256": "out-hash"`, `"output_sha256": "tampered"`)
	writeFile(t, p, raw)
	res, _ := Load(dir, "job4", expectedRefs())
	if res.Outcome != OutcomeReset {
		t.Fatalf("自校验失败必须回退, got %s", res.Outcome)
	}
}

func TestDeleteIdempotent(t *testing.T) {
	dir := t.TempDir()
	_ = Save(dir, sampleStore("job5"))
	if err := Delete(dir, "job5"); err != nil {
		t.Fatal(err)
	}
	if err := Delete(dir, "job5"); err != nil {
		t.Fatalf("重复删除应幂等, got %v", err)
	}
}

// --- 小工具，避免在测试里直接散落 os 调用 ---

func readFile(t *testing.T, p string) []byte {
	t.Helper()
	b, err := osReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func writeFile(t *testing.T, p string, b []byte) {
	t.Helper()
	if err := osWriteFile(p, b); err != nil {
		t.Fatal(err)
	}
}
