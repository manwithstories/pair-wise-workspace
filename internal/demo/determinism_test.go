package demo_test

import (
	"testing"

	"medialab/internal/demo"
)

func TestSameSeedProducesIdenticalIndexAndSegmentHashes(t *testing.T) {
	opts := demo.Options{Seed: 17, BurstLoss: 7, BandwidthKbps: []int{900, 280, 1200}}
	first, err := demo.Run(opts)
	if err != nil {
		t.Fatal(err)
	}
	second, err := demo.Run(opts)
	if err != nil {
		t.Fatal(err)
	}
	if first.IndexSHA256 != second.IndexSHA256 {
		t.Fatalf("index hashes differ: %s vs %s", first.IndexSHA256, second.IndexSHA256)
	}
	if len(first.Segments) != len(second.Segments) {
		t.Fatalf("segment count %d vs %d", len(first.Segments), len(second.Segments))
	}
	for i := range first.Segments {
		if first.Segments[i].Hash != second.Segments[i].Hash {
			t.Fatalf("segment %d hash differs", i)
		}
	}
	if !first.ExpectedPathsComplete() {
		t.Fatalf("acceptance invariants failed: %+v", first.Metrics)
	}
}
