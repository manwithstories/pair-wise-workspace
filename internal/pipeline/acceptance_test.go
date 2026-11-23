package pipeline

import (
	"context"
	"path/filepath"
	"testing"

	"mediacore/internal/fixture"
)

// TestAcceptanceFieldsMatchFixtureTruth independently verifies the demo JSON
// fields against the fixture's own ground truth, so every printed field can be
// reproduced from the generated fixture without trusting the pipeline.
func TestAcceptanceFieldsMatchFixtureTruth(t *testing.T) {
	art, err := fixture.Build(fixture.DefaultSpec(42, 0))
	if err != nil {
		t.Fatal(err)
	}
	var truthSamples int
	for _, tr := range art.Tracks {
		truthSamples += tr.SampleCount
	}
	dir := t.TempDir()
	cfg := DefaultConfig(42, 4, filepath.Join(dir, "c.json"), filepath.Join(dir, "o.mp4"))
	res, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if res.Tracks != len(art.Tracks) {
		t.Fatalf("tracks %d != truth %d", res.Tracks, len(art.Tracks))
	}
	if res.Samples != truthSamples {
		t.Fatalf("samples %d != truth %d", res.Samples, truthSamples)
	}
	if !res.OrderedEqual || res.SHA256 == "" || res.FirstErrorOffset != -1 {
		t.Fatalf("healthy fixture fields wrong: %+v", res)
	}
}
