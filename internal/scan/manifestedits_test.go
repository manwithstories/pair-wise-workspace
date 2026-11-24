package scan

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mediaroute/internal/fixtures"
	"mediaroute/internal/manifest"
)

// TestManifestEditListOverridesElst checks that an edit list declared in the
// manifest is authoritative and carried into the index as exact rationals.
func TestManifestEditListOverridesElst(t *testing.T) {
	dir := t.TempDir()
	a, err := fixtures.Build(dir, fixtures.DefaultSpec(42), fixtures.VariantNormal)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(a.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	// Replace the video edit with two manifest-declared entries.
	edited := strings.Replace(string(raw),
		`"edits": [{"segment_duration_ms": 2880, "media_time_ms": 0}]`,
		`"edits": [{"segment_duration_ms": 1000, "media_time_ms": 0}, {"segment_duration_ms": 1880, "media_time_ms": 1000}]`, 1)
	if edited == string(raw) {
		t.Fatalf("template not found in manifest:\n%s", raw)
	}
	path := filepath.Join(dir, "edited-manifest.json")
	if err := os.WriteFile(path, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := manifest.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	res, err := Run(context.Background(), m, Options{BaseDir: dir, VerifyBytes: true})
	if err != nil {
		t.Fatal(err)
	}
	video := res.Index.TrackByID(1)
	if len(video.Edits) != 2 {
		t.Fatalf("manifest edits not carried: %+v", video.Edits)
	}
	if video.Edits[0].SegmentDuration.Num != 1000 || video.Edits[0].SegmentDuration.Den != 1000 {
		t.Fatalf("first edit duration wrong: %s", video.Edits[0].SegmentDuration)
	}
	if video.Edits[1].MediaTime.Num != 1000 || video.Edits[1].MediaTime.Den != 1000 {
		t.Fatalf("second edit media time wrong: %s", video.Edits[1].MediaTime)
	}
	// Audio had no manifest edits, so it falls back to the MP4 elst.
	audio := res.Index.TrackByID(2)
	if len(audio.Edits) != 2 {
		t.Fatalf("audio should keep its elst edits: %+v", audio.Edits)
	}
}
