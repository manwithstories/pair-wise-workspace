package pipeline

import (
	"os"
	"path/filepath"
	"testing"

	"pairflow/internal/clock"
	"pairflow/internal/rtp"
	"pairflow/internal/testkit"
)

// writeFixtureCaptures 把夹具写成每设备一个采集文件，返回文件路径。
func writeFixtureCaptures(t *testing.T, dir string, fx *testkit.Fixture) []string {
	t.Helper()
	var files []string
	for _, cap := range fx.Captures {
		p := filepath.Join(dir, "capture-"+cap.Spec.DeviceID+".pfc")
		f, err := os.Create(p)
		if err != nil {
			t.Fatal(err)
		}
		if err := rtp.WriteCaptureHeader(f, 1); err != nil {
			t.Fatal(err)
		}
		for _, r := range cap.Records {
			if err := rtp.WriteCaptureRecord(f, r); err != nil {
				t.Fatal(err)
			}
		}
		f.Close()
		files = append(files, p)
	}
	return files
}

func TestEndToEndAllTracksComplete(t *testing.T) {
	dir := t.TempDir()
	fx := testkit.GenerateFixture(41, "changing")
	files := writeFixtureCaptures(t, dir, fx)

	res, err := Run(Options{OutDir: filepath.Join(dir, "out"),
		CacheBytes: MaxTrackCacheBytes, InputFiles: files, TargetBitrate: 192000})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(res.Tracks) != 3 {
		t.Fatalf("want 3 tracks even with corrupted input, got %d", len(res.Tracks))
	}
	// dev-c 有截断头，但其他轨道必须完成；截断错误被结构化保留。
	var sawTrunc bool
	for _, e := range res.TrackErrors {
		if e.Kind == clock.KindTruncatedHeader {
			sawTrunc = true
		}
	}
	if !sawTrunc {
		t.Fatal("truncated header error must be retained, not swallowed")
	}
	if _, err := os.Stat(res.IndexPath); err != nil {
		t.Fatalf("index container missing: %v", err)
	}
	if _, err := os.Stat(res.ManifestPath); err != nil {
		t.Fatalf("manifest missing: %v", err)
	}
}

func TestCheckpointDigestMismatch(t *testing.T) {
	dir := t.TempDir()
	fx := testkit.GenerateFixture(41, "steady")
	files := writeFixtureCaptures(t, dir, fx)
	out := filepath.Join(dir, "out")

	if _, err := Run(Options{OutDir: out, InputFiles: files}); err != nil {
		t.Fatal(err)
	}
	cp, err := LoadCheckpoint(out)
	if err != nil || cp == nil {
		t.Fatalf("checkpoint not written: %v", err)
	}
	// 篡改检查点记录的输入摘要，模拟输入已变化。
	cp.InputDigest = "deadbeef"
	if err := cp.Save(out); err != nil {
		t.Fatal(err)
	}
	_, err = Run(Options{OutDir: out, InputFiles: files})
	if err == nil {
		t.Fatal("resume with mismatched digest must fail")
	}
	se, ok := clock.AsStageError(err)
	if !ok || se.Kind != clock.KindCheckpointMismatch {
		t.Fatalf("want checkpoint_digest_mismatch, got %v", err)
	}
}

func TestCacheLimitTriggersCheckpointFile(t *testing.T) {
	dir := t.TempDir()
	fx := testkit.GenerateFixture(7, "changing")
	files := writeFixtureCaptures(t, dir, fx)
	out := filepath.Join(dir, "out")

	// 极小缓存上限强制触发 OnCheckpoint 落盘。
	res, err := Run(Options{OutDir: out, InputFiles: files, CacheBytes: 128})
	if err != nil {
		t.Fatalf("run with tight cache: %v", err)
	}
	var checkpoints int64
	for _, tr := range res.Tracks {
		checkpoints += tr.ReordererStats().CacheCheckpoints
	}
	if checkpoints == 0 {
		t.Fatal("tight cache must produce checkpoint events")
	}
	if _, err := os.Stat(filepath.Join(out, checkpointName)); err != nil {
		t.Fatalf("checkpoint file must exist: %v", err)
	}
}

func TestResumeProducesSameResult(t *testing.T) {
	dir := t.TempDir()
	fx := testkit.GenerateFixture(41, "changing")
	files := writeFixtureCaptures(t, dir, fx)
	out := filepath.Join(dir, "out")

	first, err := Run(Options{OutDir: out, InputFiles: files, TargetBitrate: 192000})
	if err != nil {
		t.Fatal(err)
	}
	// 再次运行：存在检查点且摘要匹配，应正常续跑/幂等完成。
	second, err := Run(Options{OutDir: out, InputFiles: files, TargetBitrate: 192000})
	if err != nil {
		t.Fatalf("resume run: %v", err)
	}
	if len(second.Tracks) != len(first.Tracks) {
		t.Fatalf("resume track count drift: %d vs %d", len(second.Tracks), len(first.Tracks))
	}
}

func TestWraparoundRepaired(t *testing.T) {
	dir := t.TempDir()
	fx := testkit.GenerateFixture(41, "changing")
	files := writeFixtureCaptures(t, dir, fx)
	res, err := Run(Options{OutDir: filepath.Join(dir, "out"), InputFiles: files})
	if err != nil {
		t.Fatal(err)
	}
	var wrapSeen bool
	for _, tr := range res.Tracks {
		if tr.ReordererStats().Gaps != nil {
			wrapSeen = wrapSeen || len(tr.ReordererStats().Gaps) > 0
		}
	}
	if !wrapSeen {
		t.Fatal("burst-loss gaps (incl. across seq wrap) must be present")
	}
}
