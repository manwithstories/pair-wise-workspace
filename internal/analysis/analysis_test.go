package analysis

import (
	"path/filepath"
	"testing"

	"mediadelivery/internal/fixture"
)

// genSegments builds a tiny on-disk segment set and returns its directory.
func genSegments(t *testing.T, profile fixture.Profile) string {
	t.Helper()
	dir := t.TempDir()
	ds, err := fixture.Generate(fixture.Options{
		WorkDir: dir, Format: "hls", Cameras: 1,
		DurationSec: 30, Profile: profile,
	})
	if err != nil {
		t.Fatal(err)
	}
	return ds.Cameras[0].Root
}

func TestDetectSilenceOnSyntheticSegment(t *testing.T) {
	root := genSegments(t, fixture.ProfileClean)
	// Slot 6 of the "stage" camera is generated with genuine silence.
	silentPath := filepath.Join(root, "primary", "seg-6.ts")
	tonePath := filepath.Join(root, "primary", "seg-1.ts")

	silent, err := DetectSilence(silentPath, DefaultSilenceConfig())
	if err != nil {
		t.Fatalf("detect silence: %v", err)
	}
	tone, err := DetectSilence(tonePath, DefaultSilenceConfig())
	if err != nil {
		t.Fatalf("detect tone: %v", err)
	}
	if silent.Channels < 1 || len(silent.ChannelsSilence) != silent.Channels {
		t.Fatalf("bad silence report: %+v", silent)
	}
	if !silent.AnySilent {
		t.Fatal("expected silence in the dedicated silent segment")
	}
	if tone.AnySilent {
		t.Fatal("tone segment was incorrectly classified as silent")
	}
}

func TestFingerprintDuplicateAndDistinct(t *testing.T) {
	root := genSegments(t, fixture.ProfileClean)
	fp1, err := FingerprintSegment(filepath.Join(root, "primary", "seg-1.ts"))
	if err != nil {
		t.Fatal(err)
	}
	// The backup representation carries the same generated content.
	fp1Backup, err := FingerprintSegment(filepath.Join(root, "backup", "seg-1.ts"))
	if err != nil {
		t.Fatal(err)
	}
	if !Similarity(fp1, fp1Backup) {
		t.Fatal("identical content across representations was not matched")
	}
	// A different segment index has distinct content.
	fpOther, err := FingerprintSegment(filepath.Join(root, "primary", "seg-3.ts"))
	if err != nil {
		t.Fatal(err)
	}
	if Similarity(fp1, fpOther) {
		t.Fatal("genuinely different clips were incorrectly merged")
	}
}

func TestSimilarityNilSafety(t *testing.T) {
	if Similarity(nil, &Fingerprint{}) {
		t.Fatal("nil fingerprint must not match")
	}
}
