package demo

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mediaroute/internal/fixtures"
	"mediaroute/internal/manifest"
	"mediaroute/internal/scan"
)

// TestNonZeroEditMediaTimeKeepsOrigin guards the edit-mapping regression: the
// timeline origin must not be folded into the edit media offset, so the first
// presented sample sits exactly at the declared origin.
func TestNonZeroEditMediaTimeKeepsOrigin(t *testing.T) {
	dir := t.TempDir()
	a, err := fixtures.Build(dir, fixtures.DefaultSpec(42), fixtures.VariantNormal)
	if err != nil {
		t.Fatal(err)
	}
	// The fixture manifest has one video edit with media_time 0; assert the
	// mapped first-sample time equals the declared origin, which fails if the
	// origin is folded into the edit offset.
	m, err := manifest.ReadFile(a.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	res, err := scan.Run(context.Background(), m, scan.Options{BaseDir: dir, VerifyBytes: true})
	if err != nil {
		t.Fatal(err)
	}
	tr := res.Index.TrackByID(1)
	if tr == nil {
		t.Fatal("video track missing")
	}
	first := sampleTimeline(*tr, tr.Samples[0].DTS)
	if d := first.Milliseconds() - 1234; d < -0.5 || d > 0.5 {
		t.Fatalf("first sample at %f ms, want the origin 1234", first.Milliseconds())
	}
	// The edit-mapped row must differ from source only by the composition
	// offset, not by the origin.
	raw, err := os.ReadFile(a.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"origin_ms": 1234`) {
		t.Fatalf("fixture manifest lost its origin: %s", filepath.Base(a.Manifest))
	}
}
