package pipeline

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"mediad/internal/checkpoint"
	"mediad/internal/codec"
	"mediad/internal/sample"
	"mediad/internal/timeline"
)

func testMedia(t *testing.T, truncate bool) *sample.Media {
	t.Helper()
	m, err := sample.Generate(sample.Config{
		Seed: 41, TargetBPS: 900_000, DriftPPM: 120,
		Wrap: true, Truncate: truncate,
	})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func runH264(t *testing.T, dir string, m *sample.Media, opts Options) *Result {
	t.Helper()
	opts.OutDir = dir
	opts.Codec = codec.H264
	opts.Media = m
	res, err := Run(opts)
	if err != nil && !errors.Is(err, ErrInterrupted) {
		t.Fatalf("Run: %v", err)
	}
	return res
}

func TestNormalRunMeetsBitrateAndAlignment(t *testing.T) {
	m := testMedia(t, false)
	res, err := Run(Options{OutDir: t.TempDir(), Codec: codec.H264, Media: m, InjectDuplicate: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Manifest.BadAUs != 0 || res.Manifest.CompleteAUPct != 100 {
		t.Fatalf("completeness bad=%d pct=%.2f", res.Manifest.BadAUs, res.Manifest.CompleteAUPct)
	}
	if absFloat(res.DeltaPct) > 5 {
		t.Fatalf("bitrate delta=%.3f%% exceeds 5", res.DeltaPct)
	}
	if res.Manifest.AlignMaxMS > 1 {
		t.Fatalf("align=%.6fms exceeds 1ms (%d ns)", res.Manifest.AlignMaxMS, res.Manifest.AlignMaxAbsNS)
	}
	if len(res.Manifest.Segments) != 4 {
		t.Fatalf("segments=%d want 4", len(res.Manifest.Segments))
	}
	if res.Manifest.DupSavedBytes == 0 || res.Manifest.RTPDuplicates == 0 {
		t.Fatalf("expected dedup and RTP duplicate stats: %+v", res.Manifest)
	}
	if res.Manifest.SilenceNS != 1_100_000_000 {
		t.Fatalf("silence=%d want 1.1s", res.Manifest.SilenceNS)
	}
}

func TestManifestHashAndResumeMatchUninterrupted(t *testing.T) {
	m := testMedia(t, false)
	baselineDir := t.TempDir()
	base, err := Run(Options{OutDir: baselineDir, Codec: codec.AV1, Media: m, InjectDuplicate: true})
	if err != nil {
		t.Fatal(err)
	}

	recoveryDir := t.TempDir()
	_, err = Run(Options{
		OutDir: recoveryDir, Codec: codec.AV1, Media: m,
		InjectDuplicate: true, InterruptAfter: 1,
	})
	if !errors.Is(err, ErrInterrupted) {
		t.Fatalf("expected interruption, got %v", err)
	}
	resumed, err := Run(Options{
		OutDir: recoveryDir, Codec: codec.AV1, Media: m,
		InjectDuplicate: true, Resume: true,
	})
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if resumed.ManifestSHA256 != base.ManifestSHA256 {
		t.Fatalf("manifest hash mismatch\nresume=%s\nbase=%s", resumed.ManifestSHA256, base.ManifestSHA256)
	}

	second, err := Run(Options{OutDir: t.TempDir(), Codec: codec.AV1, Media: m, InjectDuplicate: true})
	if err != nil {
		t.Fatal(err)
	}
	if second.ManifestSHA256 != base.ManifestSHA256 {
		t.Fatalf("repeat run hash mismatch: %s != %s", second.ManifestSHA256, base.ManifestSHA256)
	}
}

func TestTruncatedAUIsolated(t *testing.T) {
	m := testMedia(t, true)
	res, err := Run(Options{OutDir: t.TempDir(), Codec: codec.H265, Media: m})
	if err != nil {
		t.Fatalf("isolated corruption should not fail whole run: %v", err)
	}
	if res.Manifest.BadAUs != 1 {
		t.Fatalf("bad=%d want 1", res.Manifest.BadAUs)
	}
	if res.FirstErrorByte < 0 {
		t.Fatalf("first error byte not reported: %+v", res.Manifest)
	}
	if res.Manifest.CompleteAUs != sample.AUCount-1 {
		t.Fatalf("complete=%d want %d", res.Manifest.CompleteAUs, sample.AUCount-1)
	}
}

func TestCorruptCheckpointReturnsStorageExitCode(t *testing.T) {
	m := testMedia(t, false)
	dir := t.TempDir()
	if _, err := Run(Options{OutDir: dir, Codec: codec.H264, Media: m}); err != nil {
		t.Fatal(err)
	}
	journal := filepath.Join(dir, "journal-h264.json")
	if err := os.WriteFile(journal, []byte("{broken"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Run(Options{OutDir: dir, Codec: codec.H264, Media: m, Resume: true})
	if !errors.Is(err, checkpoint.ErrCorruptCheckpoint) {
		t.Fatalf("err=%v want corrupt checkpoint", err)
	}
	if code := timeline.ExitCode(err); code != 4 {
		t.Fatalf("exit=%d want 4", code)
	}
}

func absFloat(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}
