package pipeline

import (
	"testing"

	"github.com/example/mediad/internal/txn"
)

func TestRunHappyPath(t *testing.T) {
	res, err := Run(Options{
		WorkDir: t.TempDir(), Seed: 41, Loss: 3,
		VideoSamples: 12,
	})
	if err != nil {
		t.Fatal(err)
	}
	m := res.Metrics
	if m.Segments != 12 {
		t.Fatalf("segments = %d", m.Segments)
	}
	if m.HashMatched != 12 || m.HashTotal != 12 {
		t.Fatalf("hash match = %d/%d", m.HashMatched, m.HashTotal)
	}
	if m.RoundTripRate != 1.0 || m.KeyframeHitRate != 1.0 {
		t.Fatalf("rates = rt %.3f kf %.3f", m.RoundTripRate,
			m.KeyframeHitRate)
	}
	if m.RecoveredPackets != 3 {
		t.Fatalf("recovered = %d", m.RecoveredPackets)
	}
	if m.Unrecoverable != 0 {
		t.Fatalf("unrecoverable = %d", m.Unrecoverable)
	}
	if m.RejectedSegments != 2 {
		t.Fatalf("rejected = %d", m.RejectedSegments)
	}
	if m.AVOffsetMicros/1000 > 20 {
		t.Fatalf("av offset ms = %d", m.AVOffsetMicros/1000)
	}
	if m.MetadataTimeRatio > 2.0 {
		t.Fatalf("metadata time ratio = %v", m.MetadataTimeRatio)
	}
}

func TestRunCrashBeforeCommit(t *testing.T) {
	res, err := Run(Options{
		WorkDir: t.TempDir(), Seed: 41, Loss: 3,
		CrashAfter:   string(txn.CrashBeforeCommit),
		VideoSamples: 12,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Metrics.RollbackCount != 1 {
		t.Fatalf("rollback = %d, want 1", res.Metrics.RollbackCount)
	}
	if res.Metrics.HashMatched != 12 {
		t.Fatalf("post-recovery hash match = %d", res.Metrics.HashMatched)
	}
}

func TestRunIdempotentReplay(t *testing.T) {
	dir := t.TempDir()
	first, err := Run(Options{WorkDir: dir, Seed: 7, Loss: 1,
		VideoSamples: 6})
	if err != nil {
		t.Fatal(err)
	}
	// Replay reuses committed artifacts; resetManagedDirs removes them,
	// so a fresh run must still reproduce identical hashes.
	second, err := Run(Options{WorkDir: dir, Seed: 7, Loss: 1,
		VideoSamples: 6})
	if err != nil {
		t.Fatal(err)
	}
	for i := range first.Manifest.Segments {
		if first.Manifest.Segments[i].SHA256 !=
			second.Manifest.Segments[i].SHA256 {
			t.Fatalf("segment %s hash changed across runs",
				first.Manifest.Segments[i].ID)
		}
	}
}

func TestVerifyManifest(t *testing.T) {
	dir := t.TempDir()
	if _, err := Run(Options{WorkDir: dir, Seed: 1, Loss: 0,
		VideoSamples: 4}); err != nil {
		t.Fatal(err)
	}
	report, err := VerifyManifest(dir + "/artifacts/manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	if !report.AllMatch || report.HashMatched != 4 {
		t.Fatalf("report = %+v", report)
	}
	if report.Rejected != 2 {
		t.Fatalf("rejected in manifest = %d", report.Rejected)
	}
}
