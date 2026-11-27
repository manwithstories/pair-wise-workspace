package pipeline

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"relay/internal/demux"
	"relay/internal/fixture"
	"relay/internal/media"
	"relay/internal/remux"
)

func setupFixture(t *testing.T, frames int) *fixture.Outputs {
	t.Helper()
	dir := t.TempDir()
	out, err := fixture.Generate(fixture.Plan{
		Dir: dir, Frames: frames,
		Faults: []string{"crc", "truncated", "late", "duplicate", "slow", "interrupt"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestRunToCompletionDeterministic(t *testing.T) {
	fx := setupFixture(t, 400)
	dir := filepath.Dir(fx.Live)
	out := filepath.Join(dir, "out.mta")
	man := filepath.Join(dir, "manifest.json")
	ck := filepath.Join(dir, "run.ckpt")

	e, err := New(context.Background(), Options{
		InputPath: fx.Live, OutputPath: out, MapPath: fx.Map,
		CheckpointPath: ck, ManifestPath: man, MaxBuffer: 64 << 20,
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if e.Stats().PeakBuffer > 64<<20 {
		t.Fatalf("peak buffer %d exceeds 64MiB", e.Stats().PeakBuffer)
	}
	if e.Stats().Duplicates == 0 {
		t.Fatal("expected idempotent duplicates to be counted")
	}

	// 独立重读并验证。
	raw, _ := os.ReadFile(man)
	manifest := decodeManifest(t, raw)
	rep, err := remux.VerifyOutput(out, manifest)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if int64(rep.Frames) != e.Stats().Released || !rep.ManifestOK {
		t.Fatalf("verify mismatch: %+v stats=%+v", rep, e.Stats())
	}
}

func TestInterruptThenResumeCompletes(t *testing.T) {
	fx := setupFixture(t, 400)
	dir := filepath.Dir(fx.Live)
	out := filepath.Join(dir, "out.mfs")
	man := filepath.Join(dir, "manifest.json")
	ck := filepath.Join(dir, "run.ckpt")
	opts := Options{
		InputPath: fx.Live, OutputPath: out, MapPath: fx.Map,
		CheckpointPath: ck, ManifestPath: man, MaxBuffer: 64 << 20,
		InterruptAfter: 200,
	}

	e, err := New(context.Background(), opts, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Run(context.Background()); !errors.Is(err, ErrInterrupted) {
		t.Fatalf("want ErrInterrupted, got %v", err)
	}
	firstReleased := e.Stats().Released
	if firstReleased == 0 {
		t.Fatal("no frames committed before interrupt")
	}
	if !CheckpointExists(ck) {
		t.Fatal("checkpoint not written at interrupt")
	}

	// 恢复：截断 + 续跑直到 EOF。
	e2, err := New(context.Background(), opts, true)
	if err != nil {
		t.Fatalf("resume build: %v", err)
	}
	if err := e2.Run(context.Background()); err != nil {
		t.Fatalf("resume run: %v", err)
	}

	// 与一次性运行对比：最终帧数与载荷集合必须一致。
	oneShot := runOneShot(t, fx, filepath.Join(dir, "oneshot.mfs"))
	resumed, err := demux.ReadAll(mustOpenFile(t, out), out)
	if err != nil {
		t.Fatal(err)
	}
	if len(resumed) != len(oneShot) {
		t.Fatalf("frame count after resume = %d, want %d", len(resumed), len(oneShot))
	}
	for i := range oneShot {
		if oneShot[i].SHA256 != resumed[i].SHA256 ||
			oneShot[i].EventNS != resumed[i].EventNS ||
			oneShot[i].Sequence != resumed[i].Sequence {
			t.Fatalf("frame %d differs after resume", i)
		}
	}
}

func TestCorruptInputReturnsMediaFault(t *testing.T) {
	fx := setupFixture(t, 40)
	dir := filepath.Dir(fx.BadCRC)
	e, err := New(context.Background(), Options{
		InputPath: fx.BadCRC, OutputPath: filepath.Join(dir, "x.mta"),
		MapPath: fx.Map, CheckpointPath: filepath.Join(dir, "x.ckpt"),
		ManifestPath: filepath.Join(dir, "x.json"), MaxBuffer: 64 << 20,
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	err = e.Run(context.Background())
	if f := media.AsFault(err); f == nil || f.Stage != media.StageCRC {
		t.Fatalf("want CRC fault, got %v", err)
	}
}

func TestChangedMapRejectsResume(t *testing.T) {
	fx := setupFixture(t, 400)
	dir := filepath.Dir(fx.Live)
	opts := Options{
		InputPath: fx.Live, OutputPath: filepath.Join(dir, "o.mfs"),
		MapPath: fx.Map, CheckpointPath: filepath.Join(dir, "o.ckpt"),
		ManifestPath: filepath.Join(dir, "o.json"), MaxBuffer: 64 << 20,
		InterruptAfter: 100,
	}
	e, err := New(context.Background(), opts, false)
	if err != nil {
		t.Fatalf("initial New: %v", err)
	}
	if err := e.Run(context.Background()); !errors.Is(err, ErrInterrupted) {
		t.Fatalf("want interrupt, got %v", err)
	}
	// 篡改映射文件后恢复必须以 checkpoint 错误中止，且不覆盖产物。
	orig, _ := os.ReadFile(fx.Map)
	tampered := append(append([]byte(nil), orig...), '\n', ' ')
	_ = os.WriteFile(fx.Map, tampered, 0o644)
	_, err = New(context.Background(), opts, true)
	if err == nil {
		t.Fatal("resume with changed map must fail")
	}
	if f := media.AsFault(err); f == nil || f.Stage != media.StageCheckpoint {
		t.Fatalf("want checkpoint fault, got %v", err)
	}
}
