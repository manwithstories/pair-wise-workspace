package main

import (
	"testing"
)

// End-to-end recovery check across the ingest/align/commit chain: the
// seeded burst losses surface as exactly two 7-packet unrecoverable
// ranges, everything else is recovered, and damaged segments are marked
// rather than committed.
func TestPipelineRecoveryAcrossChain(t *testing.T) {
	cfg := &config{seed: 42, loss: 0.03, burst: 7, queueBytes: 65536}
	data, err := buildPipeline(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if data.unrecoverable != 14 {
		t.Fatalf("unrecoverable = %d, want 14 (two bursts of 7)", data.unrecoverable)
	}
	if data.recoverable == 0 || data.recovered != data.recoverable {
		t.Fatalf("recovered %d of %d recoverable", data.recovered, data.recoverable)
	}
	if data.duplicates == 0 {
		t.Fatal("expected duplicate packets to be counted")
	}
	if data.marked == 0 {
		t.Fatal("expected damaged segments to be marked")
	}
	for _, seg := range data.segments {
		if seg.End-seg.Start != segmentTicks {
			t.Fatalf("segment %d span %d, want %d", seg.ID, seg.End-seg.Start, segmentTicks)
		}
	}
	if len(data.avSyncMs) == 0 || p95Ms(data.avSyncMs) > 20 {
		t.Fatalf("av sync p95 = %.2f ms", p95Ms(data.avSyncMs))
	}
}

// The same seed must regenerate byte-identical segments.
func TestPipelineDeterministic(t *testing.T) {
	cfg := &config{seed: 42, loss: 0.03, burst: 7, queueBytes: 65536}
	a, err := buildPipeline(cfg)
	if err != nil {
		t.Fatal(err)
	}
	b, err := buildPipeline(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.segments) != len(b.segments) {
		t.Fatalf("segment counts differ: %d vs %d", len(a.segments), len(b.segments))
	}
	for i := range a.segments {
		if a.segments[i].Digest != b.segments[i].Digest {
			t.Fatalf("segment %d digest differs across runs", i)
		}
	}
}
