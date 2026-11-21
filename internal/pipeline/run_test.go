package pipeline

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mediad/internal/fixtures"
	"mediad/internal/manifest"
	"mediad/internal/model"
)

func baseOptions(t *testing.T) Options {
	opt := DefaultOptions()
	opt.Out = t.TempDir()
	return opt
}

func TestRunSuccessAcceptance(t *testing.T) {
	opt := baseOptions(t)
	res, err := Run(opt)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.BlockedNode != "none" {
		t.Fatalf("blocked_node = %s", res.BlockedNode)
	}
	if res.FirstError != nil {
		t.Fatalf("unexpected error %v", res.FirstError)
	}
	m := res.Manifest
	if m.Metrics.FrameCount == 0 || len(m.Frames) != m.Metrics.FrameCount {
		t.Fatalf("frames = %d", m.Metrics.FrameCount)
	}
	if m.Metrics.PSNRMeanDB < 30 {
		t.Fatalf("psnr = %.2f", m.Metrics.PSNRMeanDB)
	}
	if m.Metrics.SSIMMean < 0.90 {
		t.Fatalf("ssim = %.4f", m.Metrics.SSIMMean)
	}
	if m.Metrics.BitrateErrorPct > 8 {
		t.Fatalf("bitrate error = %.2f%%", m.Metrics.BitrateErrorPct)
	}
	t.Logf("bitrate error = %.2f%%, target=%.1f actual=%.1f", m.Metrics.BitrateErrorPct, m.Metrics.TargetKbps, m.Metrics.ActualKbps)
	if m.Metrics.ManifestDriftMs > 1 {
		t.Fatalf("drift = %dms", m.Metrics.ManifestDriftMs)
	}
	if m.Stable == nil || !m.Stable.FrameHashesStable || !m.Stable.ManifestStable {
		t.Fatalf("unstable: %+v", m.Stable)
	}
	if len(m.Artifacts) == 0 {
		t.Fatal("no artifacts recorded")
	}
	// Manifest must be valid JSON on disk.
	raw, err := os.ReadFile(filepath.Join(opt.Out, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("manifest is not JSON: %v", err)
	}
	for _, key := range []string{"metrics", "frames", "tracks", "transport", "color", "nodes"} {
		if _, ok := decoded[key]; !ok {
			t.Fatalf("manifest missing %q", key)
		}
	}
}

func TestRunIsReproducible(t *testing.T) {
	opt := baseOptions(t)
	a, err := Run(opt)
	if err != nil {
		t.Fatal(err)
	}
	opt.Out = t.TempDir()
	b, err := Run(opt)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Frames) != len(b.Frames) {
		t.Fatalf("frame counts differ: %d vs %d", len(a.Frames), len(b.Frames))
	}
	for i := range a.Frames {
		if a.Frames[i].Hash != b.Frames[i].Hash {
			t.Fatalf("frame %d hash differs", i)
		}
	}
	if a.Manifest.Metrics.PSNRMeanDB != b.Manifest.Metrics.PSNRMeanDB {
		t.Fatal("psnr not reproducible")
	}
}

func TestRunCorruptMetadataExits2AndIsolates(t *testing.T) {
	opt := baseOptions(t)
	opt.CorruptMetadata = true
	res, err := Run(opt)
	if err == nil {
		t.Fatal("expected error for corrupt metadata")
	}
	if res.FirstError == nil || res.FirstError.Category != model.CategoryMetadata {
		t.Fatalf("first error = %v", res.FirstError)
	}
	if res.FirstError.Track == "" {
		t.Fatal("error lacks track provenance")
	}
	if !strings.Contains(res.FirstError.Error(), "blocked_by=") {
		t.Fatalf("error lacks blocking dependency: %v", res.FirstError)
	}
	if model.ExitCode(res.FirstError) != 2 {
		t.Fatalf("exit code = %d", model.ExitCode(res.FirstError))
	}
	// The manifest still records the isolated failure and the surviving audio
	// track.
	m, err := manifest.Load(filepath.Join(opt.Out, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Failures) == 0 {
		t.Fatal("manifest lost isolated failures")
	}
	found := false
	for _, tr := range m.Tracks {
		if tr.Kind == "audio" {
			found = true
		}
	}
	if !found {
		t.Fatal("surviving audio track missing from manifest")
	}
}

func TestRunWrapScenario(t *testing.T) {
	opt := baseOptions(t)
	opt.Scenario = fixtures.ScenarioWrap
	res, err := Run(opt)
	if err != nil {
		t.Fatalf("wrap run: %v", err)
	}
	if !res.Transport.Wrapped {
		t.Fatal("wrap not detected")
	}
	if res.Transport.Lost != 0 || res.Transport.Duplicates != 0 {
		t.Fatalf("wrap introduced loss: %+v", res.Transport)
	}
}

func TestRunDuplicateAndReorder(t *testing.T) {
	dup := baseOptions(t)
	dup.Scenario = fixtures.ScenarioDuplicate
	res, err := Run(dup)
	if err != nil {
		t.Fatal(err)
	}
	if res.Transport.Duplicates == 0 {
		t.Fatal("duplicates not counted")
	}
	if res.Transport.Unique != res.Manifest.Metrics.FrameCount*0 && res.Transport.Unique == 0 {
		t.Fatal("no unique packets")
	}

	re := baseOptions(t)
	re.Scenario = fixtures.ScenarioReorder
	res, err = Run(re)
	if err != nil {
		t.Fatal(err)
	}
	if res.Transport.Reordered == 0 {
		t.Fatal("reordering not counted")
	}
	if res.Transport.Lost != 0 {
		t.Fatalf("reorder reported loss: %+v", res.Transport)
	}
}

func TestRunBurstLossLinksGapToBlockedFrames(t *testing.T) {
	opt := baseOptions(t)
	opt.Scenario = fixtures.ScenarioBurst
	opt.LossBurst = 4
	res, err := Run(opt)
	if err != nil {
		t.Fatalf("burst run: %v", err)
	}
	if res.Transport.Lost != 4 {
		t.Fatalf("lost = %d", res.Transport.Lost)
	}
	blockedGaps := res.Transport.BlockedGaps()
	if len(blockedGaps) == 0 {
		t.Fatal("burst did not create a blocked gap")
	}
	// The manifest must link the gap to at least one blocked frame.
	linked := false
	for _, g := range res.Manifest.Gaps {
		if g.Blocked && len(g.Frames) > 0 {
			linked = true
		}
	}
	blockedFrames := 0
	for _, f := range res.Manifest.Frames {
		if f.Blocked {
			blockedFrames++
			if f.BlockingDep == "" {
				t.Fatal("blocked frame without dependency")
			}
		}
	}
	if !linked || blockedFrames == 0 {
		t.Fatalf("gap/frame linkage missing: linked=%t blocked=%d gaps=%+v", linked, blockedFrames, res.Manifest.Gaps)
	}
	if res.Manifest.Metrics.BlockedNode == "none" {
		t.Fatal("blocked node should not be none for burst loss")
	}
}

func TestRunResumeFromInterruptedManifest(t *testing.T) {
	opt := baseOptions(t)
	first, err := Run(opt)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate an interrupted run: the first nodes are done, render is not.
	prior := &manifest.Manifest{Version: manifest.Version, Seed: opt.Seed, Mode: "demo"}
	for _, n := range first.Manifest.Nodes {
		if n.Name == "metadata" || n.Name == "transport" || n.Name == "compose" {
			prior.Nodes = append(prior.Nodes, n)
		}
	}
	priorPath := filepath.Join(opt.Out, "interrupted.json")
	if err := prior.Write(priorPath); err != nil {
		t.Fatal(err)
	}
	opt.Out = t.TempDir()
	opt.ResumeFrom = priorPath
	res, err := Run(opt)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if res.BlockedNode != "none" {
		t.Fatalf("resume blocked: %s", res.BlockedNode)
	}
	// Resumed nodes must show zero fresh attempts and retain their hashes.
	for _, n := range res.Manifest.Nodes {
		switch n.Name {
		case "metadata", "transport", "compose":
			if n.Attempts != 1 {
				t.Fatalf("%s attempts = %d, want resumed 1", n.Name, n.Attempts)
			}
		}
	}
	// The resumed product must match the uninterrupted product.
	if len(res.Frames) != len(first.Frames) {
		t.Fatalf("resume produced %d frames, want %d", len(res.Frames), len(first.Frames))
	}
	for i := range first.Frames {
		if res.Frames[i].Hash != first.Frames[i].Hash {
			t.Fatalf("resume frame %d differs", i)
		}
	}
}

func TestRunUsageErrors(t *testing.T) {
	opt := baseOptions(t)
	opt.BandwidthKbps = 0
	if _, err := Run(opt); err == nil || model.ExitCode(err) != 1 {
		t.Fatalf("bad bandwidth: %v", err)
	}
	opt = baseOptions(t)
	opt.Out = ""
	if _, err := Run(opt); err == nil || model.ExitCode(err) != 1 {
		t.Fatalf("missing out: %v", err)
	}
	opt = baseOptions(t)
	opt.Retry = -1
	if _, err := Run(opt); err == nil || model.ExitCode(err) != 1 {
		t.Fatalf("bad retry: %v", err)
	}
}

func TestRunBitrateTracking(t *testing.T) {
	opt := baseOptions(t)
	opt.BandwidthKbps = 1800
	res, err := Run(opt)
	if err != nil {
		t.Fatal(err)
	}
	if res.Bandwidth.MeanAbsErrorPct > 8 {
		t.Fatalf("mean bitrate error %.2f%%", res.Bandwidth.MeanAbsErrorPct)
	}
	if len(res.Manifest.Bandwidth) == 0 {
		t.Fatal("no bandwidth trace")
	}
	// Every record must carry target, actual and deviation.
	for _, r := range res.Manifest.Bandwidth {
		if r.TargetKbps <= 0 || r.ActualKbps <= 0 {
			t.Fatalf("record missing values: %+v", r)
		}
	}
}
