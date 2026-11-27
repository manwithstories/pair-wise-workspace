package pipeline

import (
	"os"
	"path/filepath"
	"testing"

	"fastpost/internal/hls"
	"fastpost/internal/model"
)

func TestRunHappyPathPublishesManifests(t *testing.T) {
	dir := t.TempDir()
	rep, err := Run(Config{Seed: 2601, Segments: 12, OutDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Segments != 12 || rep.Variants != 2 || rep.Aligned != 12 {
		t.Fatalf("metrics wrong: %+v", rep)
	}
	if rep.SkewTicks != 0 {
		t.Fatalf("skew = %d, want 0 after alignment", rep.SkewTicks)
	}
	if len(rep.Checksum) != 64 {
		t.Fatalf("checksum len = %d, want 64", len(rep.Checksum))
	}
	for _, name := range []string{"master.m3u8", "media-cam-a.m3u8", "media-cam-b.m3u8", "checkpoint.json"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("expected output %s: %v", name, err)
		}
	}
}

func TestRunChecksumStableAcrossRuns(t *testing.T) {
	r1, err := Run(Config{Seed: 7, Segments: 8, OutDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	r2, err := Run(Config{Seed: 7, Segments: 8, OutDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if r1.Checksum != r2.Checksum {
		t.Fatalf("checksums differ: %s vs %s", r1.Checksum, r2.Checksum)
	}
}

func TestRunDetectsInjectedFault(t *testing.T) {
	rep, err := Run(Config{
		Seed: 2601, Segments: 12, Fault: "sei-truncate:4096",
		OutDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("injected fault is detected but non-fatal, got err=%v", err)
	}
	if rep.FaultExpected != 4096 || rep.FaultFound != 4096 {
		t.Fatalf("fault expected/found = %d/%d, want 4096/4096",
			rep.FaultExpected, rep.FaultFound)
	}
	if !hasKind(rep.Faults, model.FaultSEITruncated) {
		t.Fatalf("missing SEI truncation fault in %+v", rep.Faults)
	}
}

func TestRunInterruptAndResume(t *testing.T) {
	dir := t.TempDir()
	// First invocation stops at the interruption checkpoint.
	first, err := Run(Config{
		Seed: 2601, Segments: 12, InterruptAfter: 5, Resume: true, OutDir: dir,
	})
	if !IsInterrupted(err) {
		t.Fatalf("first run should be interrupted, got rep=%+v err=%v", first, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "checkpoint.json")); err != nil {
		t.Fatalf("partial checkpoint not written: %v", err)
	}
	// No manifests yet.
	if _, err := os.Stat(filepath.Join(dir, "master.m3u8")); !os.IsNotExist(err) {
		t.Fatal("manifest must not exist before resume")
	}
	// Resume completes publication and reuses checkpointed nodes.
	second, err := Run(Config{
		Seed: 2601, Segments: 12, InterruptAfter: 5, Resume: true, OutDir: dir,
	})
	if err != nil {
		t.Fatalf("resume failed: %v", err)
	}
	if second.Aligned != 12 {
		t.Fatalf("aligned after resume = %d, want 12", second.Aligned)
	}
	if second.ResumeMS < 0 || second.ResumeMS >= 3000 {
		t.Fatalf("resume_ms = %d, want value in [0,3000)", second.ResumeMS)
	}
	if second.ReusedNodes == 0 {
		t.Fatal("resume must reuse at least one checkpointed node")
	}
	if len(second.Checksum) != 64 {
		t.Fatalf("resume checksum len = %d, want 64", len(second.Checksum))
	}
}

func TestRunCrossTrackErrorPropagation(t *testing.T) {
	// A fault on cam-a must be reported with its track name, proving errors
	// carry provenance rather than being aggregated anonymously.
	rep, _ := Run(Config{
		Seed: 2601, Segments: 12, Fault: "sei-truncate:4096",
		OutDir: t.TempDir(),
	})
	found := false
	for _, f := range rep.Faults {
		if f.Kind == model.FaultSEITruncated && f.Track == "cam-a" && f.Offset == 4096 {
			found = true
		}
	}
	if !found {
		t.Fatalf("cross-track fault provenance lost: %+v", rep.Faults)
	}
}

func TestRunRejectsBadFaultSpec(t *testing.T) {
	_, err := Run(Config{Seed: 1, Segments: 4, Fault: "nonsense", OutDir: t.TempDir()})
	if err == nil {
		t.Fatal("invalid fault spec must fail")
	}
}

func TestVerifyPublishedDetectsTampering(t *testing.T) {
	dir := t.TempDir()
	files := []hls.File{{RelPath: "master.m3u8", Content: []byte("#EXTM3U\n")}}
	want := hls.ManifestChecksum(files)
	if err := hls.Publish(dir, files); err != nil {
		t.Fatal(err)
	}
	// An intact publication verifies cleanly.
	if err := verifyPublished(dir, files, want); err != nil {
		t.Fatalf("intact publish should verify: %v", err)
	}
	// Tamper with the on-disk bytes after publication.
	if err := os.WriteFile(filepath.Join(dir, "master.m3u8"),
		[]byte("#EXTM3U\ntampered"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := verifyPublished(dir, files, want)
	if err == nil {
		t.Fatal("tampered manifest must fail verification")
	}
	in, ok := err.(*model.InputError)
	if !ok || in.Faults[0].Kind != model.FaultChecksum {
		t.Fatalf("want checksum-mismatch fault, got %v", err)
	}
}

func hasKind(faults []model.Fault, kind model.FaultKind) bool {
	for _, f := range faults {
		if f.Kind == kind {
			return true
		}
	}
	return false
}
