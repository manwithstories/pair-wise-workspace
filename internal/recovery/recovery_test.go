package recovery_test

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"mediad/internal/fault"
	"mediad/internal/fixture"
	"mediad/internal/recovery"
)

func testParams(interrupt string) recovery.Params {
	return recovery.Params{
		Seed: 41, Rate: 48000, Layout: "stereo", LUFS: -18,
		SourceRate: fixture.SourceRate, SourceChannels: fixture.SourceChannels,
		Interrupt: interrupt,
	}
}

func generateRoot(t *testing.T, damage string) string {
	t.Helper()
	root := t.TempDir()
	_, err := fixture.Generate(fixture.Options{Root: root, Seed: 41, Damage: damage})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	return root
}

func TestFullPipelineVerifyMetrics(t *testing.T) {
	root := generateRoot(t, "")
	sum, err := recovery.Run(recovery.RunOptions{Root: root, Params: testParams("")})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	verified, err := recovery.Verify(root)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if sum.SegmentSHA256 != 9 || verified.SegmentSHA256 != 9 {
		t.Fatalf("segment hashes=%d/%d", sum.SegmentSHA256, verified.SegmentSHA256)
	}
	if verified.RoundtripMatched != 12 || verified.RoundtripTotal != 12 || verified.RoundtripRate != 1 {
		t.Fatalf("roundtrip %d/%d %.3f", verified.RoundtripMatched, verified.RoundtripTotal, verified.RoundtripRate)
	}
	if verified.KeyframeMatched != 48 || verified.KeyframeTotal != 48 || verified.KeyframeHit != 1 {
		t.Fatalf("keyframes %d/%d %.3f", verified.KeyframeMatched, verified.KeyframeTotal, verified.KeyframeHit)
	}
	if !verified.ReplayMatch || verified.OutputSampleRate != 48000 || verified.Channels != 2 {
		t.Fatalf("bad verified summary: %+v", verified)
	}
	if absFloat(verified.IntegratedLUFS+18) > 0.5 {
		t.Fatalf("LUFS %.2f outside tolerance", verified.IntegratedLUFS)
	}
}

func TestInterruptResumeMatchesFullSegmentBySegment(t *testing.T) {
	fullRoot := generateRoot(t, "")
	if _, err := recovery.Run(recovery.RunOptions{Root: fullRoot, Params: testParams("")}); err != nil {
		t.Fatal(err)
	}

	recoverRoot := generateRoot(t, "")
	_, err := recovery.Run(recovery.RunOptions{Root: recoverRoot, Params: testParams("loudness")})
	if !fault.IsInterrupted(err) || fault.ExitCode(err) != 75 {
		t.Fatalf("interrupt err=%v code=%d", err, fault.ExitCode(err))
	}
	sum, err := recovery.Resume(recoverRoot)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if !sum.ResumeMatch || sum.ReusedSegments != 9 {
		t.Fatalf("resume match=%v reused=%d", sum.ResumeMatch, sum.ReusedSegments)
	}

	fullWAV := readFile(t, filepath.Join(fullRoot, "output", "audio.wav"))
	resumeWAV := readFile(t, filepath.Join(recoverRoot, "output", "audio.wav"))
	if !bytes.Equal(fullWAV, resumeWAV) {
		t.Fatal("final WAV differs after resume")
	}
	fullLedger := readFile(t, recovery.LedgerPath(fullRoot))
	resumeLedger := readFile(t, recovery.LedgerPath(recoverRoot))
	if !bytes.Equal(fullLedger, resumeLedger) {
		t.Fatal("ledger differs after resume")
	}
	for i := 1; i <= 9; i++ {
		name := filepath.Join("work", segmentArtifact(i))
		a := readFile(t, filepath.Join(fullRoot, name))
		b := readFile(t, filepath.Join(recoverRoot, name))
		if !bytes.Equal(a, b) {
			t.Fatalf("segment artifact %s differs", name)
		}
	}
}

func TestSameInputRetryReusesResults(t *testing.T) {
	root := generateRoot(t, "")
	opts := recovery.RunOptions{Root: root, Params: testParams("")}
	if _, err := recovery.Run(opts); err != nil {
		t.Fatal(err)
	}
	sum, err := recovery.Run(opts)
	if err != nil {
		t.Fatalf("retry Run: %v", err)
	}
	if sum.ReusedSegments != 9 || sum.ProcessedSegments != 0 {
		t.Fatalf("retry processed=%d reused=%d", sum.ProcessedSegments, sum.ReusedSegments)
	}
	ledger, err := recovery.LoadLedger(root)
	if err != nil || len(ledger.Records) != 9 {
		t.Fatalf("ledger records=%d err=%v", len(ledger.Records), err)
	}
}

func TestInputChangeInvalidatesDownstream(t *testing.T) {
	root := generateRoot(t, "")
	if _, err := recovery.Run(recovery.RunOptions{Root: root, Params: testParams("")}); err != nil {
		t.Fatal(err)
	}
	other, err := fixture.Generate(fixture.Options{Root: t.TempDir(), Seed: 42})
	if err != nil {
		t.Fatal(err)
	}
	changed, err := os.ReadFile(other.Segments[4])
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "input", "segments", "seg-05.m4s")
	if err := os.WriteFile(target, changed, 0o644); err != nil {
		t.Fatal(err)
	}
	sum, err := recovery.Run(recovery.RunOptions{Root: root, Params: testParams("")})
	if err != nil {
		t.Fatalf("changed-input Run: %v", err)
	}
	if sum.ReusedSegments != 0 || sum.ProcessedSegments != 9 {
		t.Fatalf("invalidation processed=%d reused=%d", sum.ProcessedSegments, sum.ReusedSegments)
	}
	if _, err := recovery.Verify(root); err != nil {
		t.Fatalf("Verify after changed input: %v", err)
	}
}

func TestMissingInitAndWrapTimestampsReachRecovery(t *testing.T) {
	missingRoot := generateRoot(t, fixture.DamageMissingInit)
	_, err := recovery.Run(recovery.RunOptions{Root: missingRoot, Params: testParams("")})
	if f, ok := fault.As(err); !ok || f.Code != fault.InitMissing {
		t.Fatalf("missing init err=%v", err)
	}
	cp, err := recovery.LoadCheckpoint(filepath.Join(missingRoot, "work", "checkpoint.json"))
	if err != nil {
		t.Fatal(err)
	}
	if cp.StageDone("fmp4") {
		t.Fatal("fmp4 stage must not be checkpointed after missing init")
	}

	wrapRoot := generateRoot(t, fixture.DamageWrapTS)
	p := testParams("")
	p.Damage = fixture.DamageWrapTS
	sum, err := recovery.Run(recovery.RunOptions{Root: wrapRoot, Params: p})
	if err != nil {
		t.Fatalf("wrap Run: %v", err)
	}
	if !sum.RTPWrapped {
		t.Fatal("expected RTP wraparound")
	}
	if _, err := recovery.Verify(wrapRoot); err != nil {
		t.Fatalf("wrap Verify: %v", err)
	}
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return data
}

func segmentArtifact(i int) string {
	if i < 10 {
		return "segment-0" + string(rune('0'+i)) + ".pcm16"
	}
	return "segment-10.pcm16"
}

func absFloat(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}
