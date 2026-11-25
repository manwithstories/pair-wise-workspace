package fixture

import (
	"testing"

	"avlab/internal/bmff"
	"avlab/internal/manifest"
	"avlab/internal/media"
)

func TestGenerateDeterministic(t *testing.T) {
	b1, err := Generate(17)
	if err != nil {
		t.Fatal(err)
	}
	b2, err := Generate(17)
	if err != nil {
		t.Fatal(err)
	}
	if len(b1.Segments) != 3*SegmentCount {
		t.Fatalf("segments = %d, want %d", len(b1.Segments), 3*SegmentCount)
	}
	for key, sum := range b1.SegmentSHA {
		if b2.SegmentSHA[key] != sum {
			t.Fatalf("segment %s hash differs across runs", key)
		}
		if b1.Segments[key] == nil {
			t.Fatalf("missing bytes for %s", key)
		}
	}
	if d := manifest.Compare(b1.DASH, b2.DASH); d != nil {
		t.Fatalf("DASH manifests differ: %s", d)
	}
	if d := manifest.Compare(b1.HLS, b2.HLS); d != nil {
		t.Fatalf("HLS manifests differ: %s", d)
	}
}

func TestGeneratedSegmentsIndexable(t *testing.T) {
	b, err := Generate(17)
	if err != nil {
		t.Fatal(err)
	}
	keyframes := 0
	for key, data := range b.Segments {
		idx, err := bmff.IndexFragment(data, 0)
		if err != nil {
			t.Fatalf("index %s: %v", key, err)
		}
		if len(idx.Keyframes) == 0 {
			t.Fatalf("%s has no keyframe", key)
		}
		keyframes += len(idx.Keyframes)
	}
	if keyframes != 3*SegmentCount {
		t.Fatalf("keyframe count = %d, want %d", keyframes, 3*SegmentCount)
	}
}

func TestDamagedCasesRejected(t *testing.T) {
	b, err := Generate(17)
	if err != nil {
		t.Fatal(err)
	}
	valid := b.Segments["mid/mid/seg-1.m4s"]
	cases := BuildDamagedCases(valid)
	if len(cases) != 4 {
		t.Fatalf("damaged cases = %d, want 4", len(cases))
	}
	for _, c := range cases {
		_, err := bmff.IndexFragment(c.Data, 0)
		if !media.IsCode(err, c.Code) {
			t.Fatalf("%s: err = %v, want code %s", c.Name, err, c.Code)
		}
	}
}

func TestManifestRoundtripsFromFixture(t *testing.T) {
	b, err := Generate(17)
	if err != nil {
		t.Fatal(err)
	}
	dashRaw, err := manifest.RenderDASH(b.DASH)
	if err != nil {
		t.Fatal(err)
	}
	parsedDASH, err := manifest.ParseDASH(dashRaw)
	if err != nil {
		t.Fatal(err)
	}
	if d := manifest.Compare(b.DASH, parsedDASH); d != nil {
		t.Fatalf("DASH roundtrip: %s", d)
	}
	hlsRaw, err := manifest.RenderHLS(b.HLS)
	if err != nil {
		t.Fatal(err)
	}
	parsedHLS, err := manifest.ParseHLS(hlsRaw)
	if err != nil {
		t.Fatal(err)
	}
	if d := manifest.Compare(b.HLS, parsedHLS); d != nil {
		t.Fatalf("HLS roundtrip: %s", d)
	}
}
