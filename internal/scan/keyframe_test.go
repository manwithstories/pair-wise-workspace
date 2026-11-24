package scan

import (
	"context"
	"testing"

	"mediaroute/internal/fixtures"
	"mediaroute/internal/manifest"
)

// TestSparseKeyframesNotFabricated checks that a track with very sparse stss
// entries yields exactly those keyframes, never an interpolated random access
// point, and that a sync sample that is unreadable is not advertised.
func TestSparseKeyframesNotFabricated(t *testing.T) {
	dir := t.TempDir()
	spec := fixtures.DefaultSpec(42)
	spec.KeyframeEvery = 24 // only 3 of 72 frames
	a, err := fixtures.Build(dir, spec, fixtures.VariantNormal)
	if err != nil {
		t.Fatal(err)
	}
	m, err := manifest.ReadFile(a.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	res, err := Run(context.Background(), m, Options{BaseDir: dir, VerifyBytes: true})
	if err != nil {
		t.Fatal(err)
	}
	tr := res.Index.TrackByID(1)
	if len(tr.Keyframes) != 3 {
		t.Fatalf("want 3 keyframes got %v", tr.Keyframes)
	}
	for _, k := range tr.Keyframes {
		if (k-1)%24 != 0 {
			t.Fatalf("fabricated keyframe %d", k)
		}
	}
	// Every keyframe must correspond to a readable sync sample.
	for _, k := range tr.Keyframes {
		s := tr.Samples[k-1]
		if !s.Sync || !s.Readable {
			t.Fatalf("keyframe %d is not a readable sync sample", k)
		}
	}
}

// TestTruncatedKeyframeNotAdvertised checks that a truncated asset does not
// advertise keyframes beyond the readable prefix.
func TestTruncatedKeyframeNotAdvertised(t *testing.T) {
	dir := t.TempDir()
	a, err := fixtures.Build(dir, fixtures.DefaultSpec(42), fixtures.VariantTruncated)
	if err != nil {
		t.Fatal(err)
	}
	m, err := manifest.ReadFile(a.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	res, _ := Run(context.Background(), m, Options{BaseDir: dir, VerifyBytes: true})
	tr := res.Index.TrackByID(1)
	firstCorrupt := int64(1 << 30)
	for _, r := range res.Index.CorruptRanges {
		if r.FirstSample < firstCorrupt {
			firstCorrupt = r.FirstSample
		}
	}
	for _, k := range tr.Keyframes {
		if k >= firstCorrupt {
			t.Fatalf("keyframe %d lies inside the corrupt range starting at %d", k, firstCorrupt)
		}
	}
}
