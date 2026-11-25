package pipeline

import (
	"errors"
	"testing"

	"fieldcut/internal/clock"
	"fieldcut/internal/manifest"
	"fieldcut/internal/recover"
)

func TestCleanMetricsVerifyAndRerunHash(t *testing.T) {
	dir := t.TempDir()
	req := Request{Case: "clean", Seed: 17, OutDir: dir}
	res, err := Run(req)
	if err != nil {
		t.Fatal(err)
	}
	if res.Code != 0 {
		t.Fatalf("code=%d", res.Code)
	}
	m, err := manifest.Verify(dir)
	if err != nil {
		t.Fatal(err)
	}
	if m.Metrics.BitrateDeltaPct > 5 || m.Metrics.BitrateDeltaPct < -5 {
		t.Fatalf("bitrate delta=%f", m.Metrics.BitrateDeltaPct)
	}
	if m.Metrics.ManifestMaxSkewNs > clock.NsPerMs {
		t.Fatalf("skew=%d", m.Metrics.ManifestMaxSkewNs)
	}
	if m.Metrics.DedupeCount != 1 || m.Metrics.SegmentCount != 4 {
		t.Fatalf("dedupe=%d segments=%d", m.Metrics.DedupeCount, m.Metrics.SegmentCount)
	}
	firstHash := res.OutputSHA256

	reres, err := Run(req)
	if err != nil {
		t.Fatal(err)
	}
	if reres.OutputSHA256 != firstHash {
		t.Fatalf("rerun hash changed: %s vs %s", reres.OutputSHA256, firstHash)
	}
}

func TestInterruptResumeHashMatchesFullRun(t *testing.T) {
	interruptDir := t.TempDir()
	res, err := Run(Request{Case: "interrupted", Seed: 17, OutDir: interruptDir, Interrupt: true})
	if !errors.Is(err, ErrSimulatedInterrupt) {
		t.Fatalf("expected interrupt error, got %v", err)
	}
	if res == nil || res.Code != ExitInterrupt {
		t.Fatalf("result=%#v", res)
	}

	resumed, err := Resume(interruptDir)
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Code != 0 {
		t.Fatalf("resume code=%d", resumed.Code)
	}
	if len(resumed.ReusedNodes) != 8 {
		t.Fatalf("reused=%v", resumed.ReusedNodes)
	}

	cleanDir := t.TempDir()
	clean, err := Run(Request{Case: "clean", Seed: 17, OutDir: cleanDir})
	if err != nil {
		t.Fatal(err)
	}
	if resumed.OutputSHA256 != clean.OutputSHA256 {
		t.Fatalf("resume hash %s != full hash %s", resumed.OutputSHA256, clean.OutputSHA256)
	}
	if _, err := manifest.Verify(interruptDir); err != nil {
		t.Fatal(err)
	}
}

func TestDamagedReportsIntervalAndTruncationJournal(t *testing.T) {
	dir := t.TempDir()
	res, err := Run(Request{Case: "damaged", Seed: 17, OutDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if res.Code != 2 || res.FirstError == nil {
		t.Fatalf("code=%d first=%#v", res.Code, res.FirstError)
	}
	if res.FirstError.Code != clock.CodeBadInterval || res.FirstError.TrackID != 30 {
		t.Fatalf("unexpected first error: %#v", res.FirstError)
	}
	if got := res.Manifest.Metrics.IntervalNs; got != 80*clock.NsPerMs {
		t.Fatalf("interval_ns=%d", got)
	}
	if len(res.Manifest.Segments) != 1 {
		t.Fatalf("expected one preserved segment, got %d", len(res.Manifest.Segments))
	}
	j, err := recover.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	rec, ok := j.Record("ingest")
	if !ok || rec.Error == nil || rec.Error.Code != clock.CodeTruncatedMP4 {
		t.Fatalf("truncation did not reach recovery journal: %#v ok=%v", rec, ok)
	}
}
