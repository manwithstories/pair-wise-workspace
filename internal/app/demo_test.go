package app

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mediameld/internal/fixture"
	"mediameld/internal/model"
)

func acceptanceOptions(out string) Options {
	return Options{
		Seed: 17, LateMS: 240, DriftPPM: 85, WrapAt: 4_294_960_000,
		VFR: []int64{24, 25, 23}, OutDir: out,
	}
}

func TestRunNormalMeetsAcceptance(t *testing.T) {
	out := filepath.Join(t.TempDir(), "demo")
	var stdout bytes.Buffer
	res, err := Run(acceptanceOptions(out), &stdout)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Tracks != 3 {
		t.Errorf("tracks=%d want 3", res.Tracks)
	}
	if res.Samples != 120 {
		t.Errorf("samples=%d want 120", res.Samples)
	}
	if res.LateReplays != 1 {
		t.Errorf("late_replays=%d want 1", res.LateReplays)
	}
	if len(res.TimelineHash) != 64 {
		t.Errorf("timeline hash length=%d", len(res.TimelineHash))
	}
	if res.SampleHashMismatches != 0 {
		t.Errorf("sample_hash_mismatches=%d want 0", res.SampleHashMismatches)
	}
	if res.BitrateDeviationPct > 0.1 {
		t.Errorf("bitrate_deviation_pct=%.4f exceeds 0.1", res.BitrateDeviationPct)
	}
	if res.QualitySamples != 120 {
		t.Errorf("quality_samples=%d want 120", res.QualitySamples)
	}
	if res.QualityMatchRatePct != 100 {
		t.Errorf("quality_match_rate=%.2f", res.QualityMatchRatePct)
	}
	if res.ManifestAlignmentMaxMS > 1 {
		t.Errorf("manifest_alignment_max_ms=%d exceeds 1", res.ManifestAlignmentMaxMS)
	}
	// Every documented field must appear on stdout.
	outText := stdout.String()
	for _, field := range []string{
		"tracks=3", "samples=120", "late_replays=1", "timeline_sha256=",
		"sample_hash_mismatches=0", "target_bitrate_bps=", "actual_bitrate_bps=",
		"bitrate_deviation_pct=", "quality_samples=120", "manifest_alignment_max_ms=",
	} {
		if !strings.Contains(outText, field) {
			t.Errorf("stdout missing %q\n%s", field, outText)
		}
	}
	// All writes must stay inside the output directory.
	if _, err := os.Stat(filepath.Join(out, "manifest.json")); err != nil {
		t.Fatalf("manifest missing: %v", err)
	}
}

func TestRunIsDeterministicAcrossOutputDirs(t *testing.T) {
	a := filepath.Join(t.TempDir(), "a")
	b := filepath.Join(t.TempDir(), "b")
	var outA, outB bytes.Buffer
	resA, err := Run(acceptanceOptions(a), &outA)
	if err != nil {
		t.Fatal(err)
	}
	resB, err := Run(acceptanceOptions(b), &outB)
	if err != nil {
		t.Fatal(err)
	}
	if resA.TimelineHash != resB.TimelineHash {
		t.Fatalf("timeline hash differs:\n%s\n%s", resA.TimelineHash, resB.TimelineHash)
	}
	if outA.String() != outB.String() {
		t.Fatalf("report differs:\n%s\n%s", outA.String(), outB.String())
	}
}

func TestRunCorruptCase(t *testing.T) {
	out := filepath.Join(t.TempDir(), "corrupt")
	opts := acceptanceOptions(out)
	opts.Case = "corrupt-klv"
	var stdout bytes.Buffer
	res, err := Run(opts, &stdout)
	if err != nil {
		t.Fatalf("corrupt run must isolate, not fail: %v", err)
	}
	if res.ErrorCode != string(model.CodeKLVLength) {
		t.Fatalf("error_code=%q", res.ErrorCode)
	}
	if res.RecoveredTracks != 2 {
		t.Fatalf("recovered_tracks=%d want 2", res.RecoveredTracks)
	}
	if res.ExitCode != 2 {
		t.Fatalf("exit_code=%d want 2", res.ExitCode)
	}
	if !strings.Contains(stdout.String(), "exit_code=2") {
		t.Fatalf("stdout missing exit code:\n%s", stdout.String())
	}
	// A damaged input must not leave a manifest that could pass as complete.
	if _, err := os.Stat(filepath.Join(out, "manifest.json")); err == nil {
		t.Fatal("corrupt run left a manifest")
	}
}

func TestRunUnknownCaseIsUsageError(t *testing.T) {
	opts := acceptanceOptions(t.TempDir())
	opts.Case = "nope"
	_, err := Run(opts, &bytes.Buffer{})
	if err == nil {
		t.Fatal("unknown case accepted")
	}
	if model.ExitCode(err) != 1 {
		t.Fatalf("exit=%d want 1", model.ExitCode(err))
	}
}

func TestFixtureSidecarHashesMatchFiles(t *testing.T) {
	out := filepath.Join(t.TempDir(), "demo")
	if _, err := Run(acceptanceOptions(out), &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(out, "fixtures")
	raw, err := os.ReadFile(filepath.Join(dir, "fixtures.json"))
	if err != nil {
		t.Fatal(err)
	}
	var sc struct {
		Files []struct {
			Name string `json:"name"`
			Size int64  `json:"size"`
			Hash string `json:"sha256"`
		} `json:"files"`
	}
	if err := json.Unmarshal(raw, &sc); err != nil {
		t.Fatal(err)
	}
	if len(sc.Files) == 0 {
		t.Fatal("sidecar lists no files")
	}
	for _, f := range sc.Files {
		info, err := os.Stat(filepath.Join(dir, f.Name))
		if err != nil {
			t.Fatalf("%s: %v", f.Name, err)
		}
		if info.Size() != f.Size {
			t.Errorf("%s size %d != sidecar %d", f.Name, info.Size(), f.Size)
		}
		if info.Size() > 8<<20 {
			t.Errorf("%s exceeds 8 MiB", f.Name)
		}
	}
}

func TestFixtureGenerationStable(t *testing.T) {
	opts := fixture.Options{Seed: 17, DriftPPM: 85, WrapAt: 4_294_960_000, VFR: []int64{24, 25, 23}}
	a, err := fixture.Generate(t.TempDir(), opts)
	if err != nil {
		t.Fatal(err)
	}
	b, err := fixture.Generate(t.TempDir(), opts)
	if err != nil {
		t.Fatal(err)
	}
	for _, pair := range [][2]string{{a.StudioMP4, b.StudioMP4}, {a.CameraMXF, b.CameraMXF}, {a.RTPStream, b.RTPStream}} {
		da, _ := os.ReadFile(pair[0])
		db, _ := os.ReadFile(pair[1])
		if !bytes.Equal(da, db) {
			t.Fatalf("%s differs between runs with the same seed", filepath.Base(pair[0]))
		}
	}
}

func TestRotatedCameraMetadataIsAdjudicated(t *testing.T) {
	out := filepath.Join(t.TempDir(), "demo")
	if _, err := Run(acceptanceOptions(out), &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(out, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Tracks []struct {
			ID       string `json:"id"`
			Role     string `json:"role"`
			Rotation int    `json:"rotation"`
			Crop     string `json:"crop"`
			Color    string `json:"color"`
		} `json:"tracks"`
		Metadata []struct {
			TrackID  string `json:"track_id"`
			Property string `json:"property"`
			Source   string `json:"source"`
			Original string `json:"original"`
			Chosen   string `json:"chosen"`
			Why      string `json:"why"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	var videoFound bool
	for _, tr := range m.Tracks {
		if tr.Role != "video-main" {
			continue
		}
		videoFound = true
		if tr.Rotation != 90 {
			t.Errorf("video rotation=%d want 90", tr.Rotation)
		}
		if !strings.Contains(tr.Crop, "632") {
			t.Errorf("video crop=%q lacks 632 width", tr.Crop)
		}
	}
	if !videoFound {
		t.Fatal("no video-main track in manifest")
	}
	// Every adjudicated property must retain its original value and reason.
	for _, md := range m.Metadata {
		if md.Original == "" || md.Why == "" || md.Source == "" {
			t.Errorf("incomplete decision record: %+v", md)
		}
	}
}

func TestRTPWrapAndDriftAreExercised(t *testing.T) {
	// With the wrap point just below the 32-bit ceiling and an hour of ticks,
	// the transport stream must actually cross the wrap boundary.
	out := filepath.Join(t.TempDir(), "demo")
	if _, err := Run(acceptanceOptions(out), &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(out, "fixtures", "fixtures.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) == 0 {
		t.Fatal("empty sidecar")
	}
	// The demo would have failed with ERR_CLOCK if calibration did not
	// recover a monotonic timeline, so a successful run above is the proof.
}

func TestManifestRecordsSourceFingerprintsAndGaps(t *testing.T) {
	out := filepath.Join(t.TempDir(), "demo")
	if _, err := Run(acceptanceOptions(out), &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(out, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Sources []struct {
			Path        string `json:"path"`
			Fingerprint string `json:"fingerprint"`
		} `json:"sources"`
		Gaps     []json.RawMessage `json:"gaps"`
		Metadata []struct {
			TrackID  string `json:"track_id"`
			Property string `json:"property"`
			Source   string `json:"source"`
			Chosen   string `json:"chosen"`
		} `json:"metadata"`
		Quality struct {
			Matched int `json:"matched"`
			Total   int `json:"total"`
		} `json:"quality"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if len(m.Sources) != 2 {
		t.Fatalf("sources=%d want 2", len(m.Sources))
	}
	for _, s := range m.Sources {
		if len(s.Fingerprint) < 32 {
			t.Errorf("source %s lacks a fingerprint", s.Path)
		}
	}
	// The complete camera stream has no missing sequence numbers.
	if len(m.Gaps) != 0 {
		t.Errorf("unexpected gaps=%d", len(m.Gaps))
	}
	// Crop and rotation must both appear with a reason.
	var sawRotation, sawCrop bool
	for _, md := range m.Metadata {
		if md.Property == "rotation" && md.Source == "track" {
			sawRotation = true
		}
		if md.Property == "crop" && strings.Contains(md.Chosen, "632") {
			sawCrop = true
		}
	}
	if !sawRotation || !sawCrop {
		t.Fatalf("metadata trail incomplete: rotation=%v crop=%v", sawRotation, sawCrop)
	}
	if m.Quality.Matched != m.Quality.Total || m.Quality.Total != 120 {
		t.Fatalf("quality=%+v", m.Quality)
	}
}

func TestResumeCheckpointRecordedByDemo(t *testing.T) {
	out := filepath.Join(t.TempDir(), "demo")
	if _, err := Run(acceptanceOptions(out), &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(out, "delivery.ckpt"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) < 2 || lines[1] != "done" {
		t.Fatalf("checkpoint not marked done: %q", string(raw))
	}
}

func TestRunCropConflictCase(t *testing.T) {
	out := filepath.Join(t.TempDir(), "crop")
	opts := acceptanceOptions(out)
	opts.Case = "crop-conflict"
	var stdout bytes.Buffer
	res, err := Run(opts, &stdout)
	if err == nil {
		t.Fatal("over-range policy crop accepted")
	}
	if code, _ := model.CodeOf(err); code != model.CodeCropRange {
		t.Fatalf("code=%v", code)
	}
	if res.ExitCode != 2 {
		t.Fatalf("exit=%d want 2", res.ExitCode)
	}
	if !strings.Contains(stdout.String(), "conflict_track=") {
		t.Fatalf("conflict object not named:\n%s", stdout.String())
	}
	if _, statErr := os.Stat(filepath.Join(out, "manifest.json")); statErr == nil {
		t.Fatal("failed run left a manifest")
	}
}
