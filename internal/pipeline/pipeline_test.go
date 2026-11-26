package pipeline

import (
	"context"
	"testing"
	"time"

	"bq/internal/ffmpeg"
	"bq/internal/fixture"
	"bq/internal/model"
)

func requireFFmpeg(t *testing.T) context.Context {
	t.Helper()
	if _, _, err := ffmpeg.Binaries(); err != nil {
		t.Skipf("ffmpeg unavailable: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func shortOptions(seed uint64) fixture.Options {
	return fixture.Options{
		Seed: seed, DurationS: 20, LossRate: 0.05, InjectWrap: true,
		InjectDup: true, InjectTrunc: true, PacketCadenceS: 1, GroupSize: 5,
		AnchorConflicts: 1,
	}
}

func TestPipelineEndToEnd(t *testing.T) {
	ctx := requireFFmpeg(t)
	dir := t.TempDir()
	m, err := Generate(ctx, shortOptions(41), dir)
	if err != nil {
		t.Fatal(err)
	}
	runner := NewRunner(dir)
	res, errs := runner.Run(ctx, m)
	if len(errs) == 0 {
		t.Fatal("injected anomalies must produce error records")
	}
	if model.HasFatal(errs) {
		t.Fatalf("recoverable fixture must not be fatal: %v", errs)
	}
	// Wrap and loss must both reach the evidence report.
	if !res.Report.Propagation.WrapObserved {
		t.Fatal("PTS wrap did not propagate to verify")
	}
	if !res.Report.Propagation.LossObserved {
		t.Fatal("packet loss did not propagate to verify")
	}
	if res.Report.WrapEvents == 0 {
		t.Fatal("wrap event count not reported")
	}
	if len(res.Frames) == 0 || res.FrameHash == "" {
		t.Fatal("frames not exported")
	}
	if res.Metrics.LatencyP95Ms <= 0 {
		t.Fatal("p95 latency not measured")
	}
}

func TestPipelineDeterministicFrameHash(t *testing.T) {
	ctx := requireFFmpeg(t)
	dirA := t.TempDir()
	dirB := t.TempDir()
	opts := shortOptions(41)

	mA, err := Generate(ctx, opts, dirA)
	if err != nil {
		t.Fatal(err)
	}
	mB, err := Generate(ctx, opts, dirB)
	if err != nil {
		t.Fatal(err)
	}
	resA, errsA := NewRunner(dirA).Run(ctx, mA)
	resB, errsB := NewRunner(dirB).Run(ctx, mB)

	if resA.FrameHash != resB.FrameHash {
		t.Fatalf("frame hash differs across same-seed runs:\n%s\n%s",
			resA.FrameHash, resB.FrameHash)
	}
	if len(errsA) != len(errsB) {
		t.Fatal("error count differs across same-seed runs")
	}
	for i := range errsA {
		if errsA[i] != errsB[i] {
			t.Fatalf("error ordering/content differs at %d:\n%+v\n%+v", i, errsA[i], errsB[i])
		}
	}
}

func TestPipelineFatalCorruptManifest(t *testing.T) {
	ctx := requireFFmpeg(t)
	dir := t.TempDir()
	m, err := Generate(ctx, shortOptions(9), dir)
	if err != nil {
		t.Fatal(err)
	}
	// Corrupt a stream codec: the manifest would fail strict validation on
	// re-parse, but here we mutate the in-memory document to an unsupported
	// codec and confirm a fatal record is produced downstream.
	m.Streams[0].Codec = "vp9"
	res, _ := NewRunner(dir).Run(ctx, m)
	if !model.HasFatal(res.AllErrors) {
		t.Fatal("unsupported codec must yield a fatal record")
	}
	if res.Report.ExitCode() != 2 {
		t.Fatalf("unrecoverable input must exit 2, got %d", res.Report.ExitCode())
	}
}
