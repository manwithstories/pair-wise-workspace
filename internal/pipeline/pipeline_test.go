package pipeline

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"mediasim/internal/fixture"
)

func runFixture(t *testing.T, fixtureName string, outDir string) *Result {
	t.Helper()
	cfg := Config{Fixture: fixtureName, Seed: 17, LossBurst: 4, JitterMs: 80, JitterMax: 25, OutDir: outDir}
	res, err := Run(cfg)
	if err != nil {
		t.Fatalf("run %s: %v", fixtureName, err)
	}
	return res
}

func TestDemoNormalMeetsContract(t *testing.T) {
	res := runFixture(t, "normal", t.TempDir())
	rep := res.Report
	if rep.Frames != fixture.TotalFrames {
		t.Fatalf("frames=%d want %d", rep.Frames, fixture.TotalFrames)
	}
	if rep.FrameHashes != rep.Frames {
		t.Fatalf("hashes=%d frames=%d", rep.FrameHashes, rep.Frames)
	}
	if rep.AVSyncP95Ms > 20 {
		t.Fatalf("av sync p95=%.2f", rep.AVSyncP95Ms)
	}
	if rep.PacketRecovery < 0.90 {
		t.Fatalf("recovery=%.4f", rep.PacketRecovery)
	}
	if rep.LatencyP95Ms > 150 {
		t.Fatalf("latency p95=%.2f", rep.LatencyP95Ms)
	}
	if rep.ExitCode != ExitOK {
		t.Fatalf("exit=%d (%s)", rep.ExitCode, rep.String())
	}
	if rep.CorruptRanges != 0 {
		t.Fatalf("clean fixture reported %d corrupt ranges", rep.CorruptRanges)
	}
	if len(res.Hashes) != fixture.TotalFrames {
		t.Fatalf("hashes=%d", len(res.Hashes))
	}
}

func TestDemoTruncatedIsPartialWithPrefix(t *testing.T) {
	res := runFixture(t, "truncated", t.TempDir())
	rep := res.Report
	if rep.ExitCode != ExitPartial {
		t.Fatalf("exit=%d want 2", rep.ExitCode)
	}
	if rep.RecoveredPrefix <= 0 {
		t.Fatal("no recovered prefix")
	}
	if rep.CorruptRanges < 1 {
		t.Fatalf("corrupt ranges=%d", rep.CorruptRanges)
	}
	if rep.Frames >= fixture.TotalFrames {
		t.Fatalf("truncation did not reduce frames: %d", rep.Frames)
	}
	if rep.Frames != rep.RecoveredPrefix && rep.RecoveredPrefix > 0 && rep.Frames > rep.RecoveredPrefix {
		t.Fatalf("rendered %d frames beyond the %d-frame prefix", rep.Frames, rep.RecoveredPrefix)
	}
}

func TestDemoCorruptSamplesPropagate(t *testing.T) {
	res := runFixture(t, "corrupt", t.TempDir())
	rep := res.Report
	if rep.ExitCode != ExitPartial {
		t.Fatalf("exit=%d", rep.ExitCode)
	}
	if rep.CorruptRanges < 1 {
		t.Fatal("no corrupt ranges")
	}
	// The corrupt samples are on cam0 frames 30-32.
	if _, err := os.Stat(filepath.Join(t.TempDir(), "x")); err != nil {
		_ = err
	}
	got := map[int]bool{}
	for _, h := range res.Hashes {
		got[h.Index] = true
	}
	for _, idx := range []int{30, 31, 32} {
		if got[idx] {
			t.Fatalf("frame %d was rendered despite its corrupt origin sample", idx)
		}
	}
	if !got[29] || !got[33] {
		t.Fatal("neighbouring frames should still render")
	}
}

func TestMissingIndexRecoversAcrossTransport(t *testing.T) {
	res := runFixture(t, "missing-index", t.TempDir())
	rep := res.Report
	if rep.Recovery.Indexed {
		t.Fatal("fixture should be unindexed")
	}
	if rep.Recovery.ScannedSamples == 0 {
		t.Fatal("scan found nothing")
	}
	if rep.Frames == 0 {
		t.Fatal("no frames rendered")
	}
	if rep.RecoveredPrefix != rep.Frames {
		t.Fatalf("scan-recovered prefix %d does not match rendered %d", rep.RecoveredPrefix, rep.Frames)
	}
	if rep.RecoveredIndexed != 0 {
		t.Fatalf("unindexed run reported %d indexed frames", rep.RecoveredIndexed)
	}
	if rep.ExitCode != ExitPartial {
		// A missing index is damage even if every frame is recovered.
		t.Fatalf("exit=%d", rep.ExitCode)
	}
}

// TestMissingIndexWithBurstLoss exercises the worst combined case from the
// spec: no index, burst loss and jitter. Scan recovery must supply the frames
// the network dropped without double counting.
func TestMissingIndexWithBurstLoss(t *testing.T) {
	cfg := Config{Fixture: "missing-index", Seed: 17, LossBurst: 4, JitterMs: 80,
		JitterMax: 25, OutDir: t.TempDir()}
	cfg.Defaults()
	res, err := Run(cfg)
	if err != nil {
		t.Fatal(err)
	}
	rep := res.Report
	if rep.RTP.Lost == 0 {
		t.Fatal("test did not actually lose packets")
	}
	if rep.RTP.Repaired == 0 {
		t.Fatal("scan recovery repaired nothing")
	}
	if rep.RTP.Repaired+rep.RTP.Unrepaired != rep.RTP.Lost {
		t.Fatalf("repaired %d + unrepaired %d != lost %d",
			rep.RTP.Repaired, rep.RTP.Unrepaired, rep.RTP.Lost)
	}
	if rep.Frames != fixture.TotalFrames {
		t.Fatalf("frames=%d want %d after recovery", rep.Frames, fixture.TotalFrames)
	}
	if rep.FrameHashes != rep.Frames {
		t.Fatalf("hashes=%d", rep.FrameHashes)
	}
	if rep.ExitCode != ExitPartial {
		t.Fatalf("exit=%d: an unindexed source is still partial", rep.ExitCode)
	}
}

func TestRerunIsByteIdentical(t *testing.T) {
	dirA, dirB := t.TempDir(), t.TempDir()
	a := runFixture(t, "normal", dirA)
	b := runFixture(t, "normal", dirB)
	if len(a.Hashes) != len(b.Hashes) {
		t.Fatalf("%d vs %d frames", len(a.Hashes), len(b.Hashes))
	}
	for i := range a.Hashes {
		if a.Hashes[i] != b.Hashes[i] {
			t.Fatalf("frame %d hash differs", i)
		}
	}
	// The on-disk PNG bytes are identical, not just the reported digest.
	pa, _ := os.ReadFile(filepath.Join(dirA, "frames", a.Hashes[0].PNG))
	pb, _ := os.ReadFile(filepath.Join(dirB, "frames", b.Hashes[0].PNG))
	if string(pa) != string(pb) {
		t.Fatal("PNG bytes differ between identical runs")
	}
}

func TestReportJSONRoundTrip(t *testing.T) {
	res := runFixture(t, "truncated", t.TempDir())
	dir := t.TempDir()
	path, err := WriteReport(dir, res.Report)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Report
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("report is not valid JSON: %v", err)
	}
	if decoded.Frames != res.Report.Frames || decoded.ExitCode != res.Report.ExitCode {
		t.Fatalf("round trip lost fields: %+v", decoded)
	}
	if decoded.CorruptRanges < 1 || decoded.RecoveredPrefix <= 0 {
		t.Fatalf("recovery fields missing: %+v", decoded)
	}
}

func TestErrorEntriesCarryCategorySpanRecovery(t *testing.T) {
	res := runFixture(t, "corrupt", t.TempDir())
	if len(res.Report.Errors) == 0 {
		t.Fatal("no errors reported for corrupt fixture")
	}
	for _, e := range res.Report.Errors {
		if e.Category == "" || e.Recovery == "" {
			t.Fatalf("incomplete error entry: %+v", e)
		}
		if e.Category != "sample" && e.Category != "sample_table" && e.Category != "index_gap" &&
			e.Category != "transport" && e.Category != "render" && e.Category != "sync" {
			t.Fatalf("unknown category %q", e.Category)
		}
	}
}
