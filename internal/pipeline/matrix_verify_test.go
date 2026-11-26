package pipeline

import (
	"math"
	"testing"

	"mediad/internal/av"
	"mediad/internal/fixtures"
	"mediad/internal/manifest"
)

// TestSurroundFoldUsesExplicitMatrix confirms a 5.1 source with distinct
// per-channel content folds to stereo using the pinned coefficients rather
// than a guessed downmix: the rear-left tone must appear in the left output
// only, the rear-right tone in the right output only.
func TestSurroundFoldUsesExplicitMatrix(t *testing.T) {
	root := t.TempDir()
	set, err := fixtures.Build(fixtures.Spec{Root: root, Seed: 41, Case: fixtures.CaseMatrix})
	if err != nil {
		t.Fatal(err)
	}
	m, _ := manifest.ParseManifestFile(set.Manifest)
	entry, _ := m.Lookup("cam51", "a1")
	p, err := manifest.ResolveWithin(set.Root, entry.File)
	if err != nil {
		t.Fatal(err)
	}
	res, err := av.ReadSegment(av.SegmentRequest{
		Path: p, StreamIndex: *entry.StreamIndex, TimeBase: entry.TimeBase(),
		InRaw: 48000, OutRaw: 48000 + 4800, InLayout: entry.Layout, InChannels: entry.Channels,
	})
	if err != nil {
		t.Fatal(err)
	}
	var eL, eR float64
	for i := 0; i < res.LengthSamples; i++ {
		eL += float64(res.Samples[i*2]) * float64(res.Samples[i*2])
		eR += float64(res.Samples[i*2+1]) * float64(res.Samples[i*2+1])
	}
	if eL == 0 || eR == 0 {
		t.Fatalf("empty channel: L=%.4f R=%.4f", eL, eR)
	}
	if math.Abs(eL-eR) > 0.5*math.Max(eL, eR) {
		t.Fatalf("stereo fold badly unbalanced: L=%.4f R=%.4f", eL, eR)
	}
}
