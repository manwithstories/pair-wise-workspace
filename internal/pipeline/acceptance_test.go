package pipeline

import "testing"

func TestDemo30SecondAcceptanceMetrics(t *testing.T) {
	res, err := RunDemo(Options{Work: t.TempDir() + "/accept", Seed: 17, Seconds: 30, TargetKbps: 1200})
	if err != nil {
		t.Fatal(err)
	}
	s := res.Manifest.Stats
	if s.TargetKbps != 1200 {
		t.Fatalf("target_kbps=%d", s.TargetKbps)
	}
	if s.ActualKbps < 1140 || s.ActualKbps > 1260 {
		t.Fatalf("actual_kbps=%d outside 1140..1260", s.ActualKbps)
	}
	if s.DeviationPct > 5 {
		t.Fatalf("deviation_pct=%.2f > 5", s.DeviationPct)
	}
	if s.DecodeErrors != 0 {
		t.Fatalf("decode_errors=%d", s.DecodeErrors)
	}
	if s.SilenceRegions != 2 {
		t.Fatalf("silence_regions=%d", s.SilenceRegions)
	}
	if s.DuplicateSegments != 1 {
		t.Fatalf("duplicate_segments=%d", s.DuplicateSegments)
	}
	if s.ManifestErrorMS > 42 {
		t.Fatalf("manifest_error_ms=%.1f > 42", s.ManifestErrorMS)
	}
	r := res.Manifest.Recovery
	if r == nil {
		t.Fatal("missing recovery stats")
	}
	if r.DuplicatePackets == 0 || r.ReorderedPackets == 0 || r.RecoveredPackets == 0 {
		t.Fatalf("recovery packet counters must be positive: %+v", r)
	}
	if !r.CacheHit || !r.SourceOK || !r.SHA256OK {
		t.Fatalf("recovery verification failed: %+v", r)
	}
}
