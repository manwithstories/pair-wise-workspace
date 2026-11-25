package pipeline

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"mediad/internal/adapter"
	"mediad/internal/fixture"
	"mediad/internal/media"
)

func gen(t *testing.T, opts fixture.Options) string {
	t.Helper()
	root := t.TempDir()
	opts.Root = root
	if _, err := fixture.Generate(opts); err != nil {
		t.Fatalf("generate: %v", err)
	}
	return root
}

func TestCleanRunProducesVerifiedOutput(t *testing.T) {
	root := gen(t, fixture.Options{Seed: 41, Profile: fixture.ProfileClean, Cams: 4, Segments: 60})
	outDir := filepath.Join(root, "out")
	cfg := Config{Root: root, OutDir: outDir, Container: adapter.ContainerCMAF, Limits: media.DefaultLimits()}
	rep, err := Run(cfg)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.OutputSamples != 4*60*4 {
		t.Fatalf("output_samples=%d want %d", rep.OutputSamples, 4*60*4)
	}
	if !rep.HashMatch {
		t.Fatal("hash_match false")
	}
	if rep.SyncP95MS > 20 {
		t.Fatalf("sync_p95_ms=%.3f", rep.SyncP95MS)
	}
	if _, err := os.Stat(filepath.Join(outDir, "output.mp4")); err != nil {
		t.Fatalf("output.mp4 missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outDir, "output.index.json")); err != nil {
		t.Fatalf("source index missing: %v", err)
	}
}

func TestDriftCorrectionKeepsSync(t *testing.T) {
	root := gen(t, fixture.Options{Seed: 41, Profile: fixture.ProfileClean, Cams: 4, Segments: 60,
		DriftPPM: 120, VFR: true, WrapPTS: 8589900000})
	rep, err := Run(Config{Root: root, Container: adapter.ContainerCMAF})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.SyncP95MS > 20 {
		t.Fatalf("sync_p95_ms=%.3f exceeds 20 under drift+VFR+wrap", rep.SyncP95MS)
	}
	if rep.OutputSamples != 4*60*4 {
		t.Fatalf("samples=%d", rep.OutputSamples)
	}
}

func TestCorruptRunLeavesNoOutput(t *testing.T) {
	root := gen(t, fixture.Options{Seed: 41, Profile: fixture.ProfileCorrupt, Cams: 4, Segments: 60,
		TruncateMoof: 17})
	outDir := filepath.Join(root, "out")
	rep, err := Run(Config{Root: root, OutDir: outDir, Container: adapter.ContainerCMAF})
	if err == nil {
		t.Fatal("expected corrupt run to fail")
	}
	if !media.Isolated(err) {
		t.Fatalf("corruption should be isolated, got code %s", media.CodeOf(err))
	}
	if rep.RejectedSegments != 1 {
		t.Fatalf("rejected_segments=%d want 1", rep.RejectedSegments)
	}
	if rep.ErrCode != string(media.CodeTruncatedBox) {
		t.Fatalf("err_code=%s", rep.ErrCode)
	}
	if _, statErr := os.Stat(filepath.Join(outDir, "output.mp4")); statErr == nil {
		t.Fatal("corrupt run left output.mp4 behind")
	}
}

func TestRecoverRunByteExact(t *testing.T) {
	root := gen(t, fixture.Options{Seed: 41, Profile: fixture.ProfileRecover, Cams: 4, Segments: 60,
		Loss: 0.03, DriftPPM: 120, WrapPTS: 8589900000, VFR: true})
	rep, err := Run(Config{Root: root, Mode: ModeRTP})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.RecoveredPackets == 0 {
		t.Fatal("recovered_packets=0")
	}
	if rep.LossRecoveryPct < 95 {
		t.Fatalf("loss_recovery_pct=%.2f", rep.LossRecoveryPct)
	}
	if rep.LatencyP95MS > 100 {
		t.Fatalf("latency_p95_ms=%.2f", rep.LatencyP95MS)
	}
	if !rep.HashMatch {
		t.Fatal("hash_match false on recovery run")
	}
	if rep.OutputSamples != 4*60*4 {
		t.Fatalf("output_samples=%d want %d (all frames must survive recovery)",
			rep.OutputSamples, 4*60*4)
	}
	// Every recovered packet's bytes must survive into output. Read the index
	// and confirm each recorded sample hash matches the fixture payload for
	// its camera and frame, proving recovery restored the exact bytes.
	indexData, err := os.ReadFile(rep.IndexPath)
	if err != nil {
		t.Fatal(err)
	}
	var idx struct {
		Entries []struct {
			Track struct {
				Camera string `json:"camera"`
			} `json:"track"`
			Sample int    `json:"sample"`
			Hash   string `json:"hash"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(indexData, &idx); err != nil {
		t.Fatal(err)
	}
	if len(idx.Entries) != rep.OutputSamples {
		t.Fatalf("index entries=%d output_samples=%d", len(idx.Entries), rep.OutputSamples)
	}
	// Build the set of hashes the fixture deterministically produced; every
	// output sample must be one of them and all must appear.
	want := make(map[string]bool)
	for _, cam := range []string{"cam1", "cam2", "cam3", "cam4"} {
		camera := fixture.Camera{Name: cam, Role: "main"}
		for f := 0; f < 60*4; f++ {
			p := fixture.RTPSamplePayload(camera, f/4, f%4)
			sum := sha256.Sum256(p)
			want[hex.EncodeToString(sum[:])] = false
		}
	}
	for _, e := range idx.Entries {
		if _, ok := want[e.Hash]; !ok {
			t.Fatalf("output sample hash not from fixture (camera %s sample %d)", e.Track.Camera, e.Sample)
		}
		want[e.Hash] = true
	}
	for h, seen := range want {
		if !seen {
			t.Fatalf("fixture sample %s missing from output", h[:12])
		}
	}
}

func TestTrackConflictRejectsSubmission(t *testing.T) {
	// Two cameras mapped to the same key must be rejected before writing.
	set := adapter.NewTrackSet()
	k := map[uint32]media.TrackKey{1: {Camera: "cam1", Language: "und", Codec: "avc1", Role: "main"}}
	if err := set.Add("a", k); err != nil {
		t.Fatal(err)
	}
	if err := set.Add("b", k); err == nil {
		t.Fatal("expected cross-source conflict")
	}
}
