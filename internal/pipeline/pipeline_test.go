package pipeline

import (
	"encoding/json"
	"strings"
	"testing"

	"mediaflow/internal/clock"
	"mediaflow/internal/repair"
)

func testConfig() Config {
	cfg := DefaultConfig()
	return cfg
}

func TestBuildSourcesDeterministic(t *testing.T) {
	a, err := BuildSources(testConfig())
	if err != nil {
		t.Fatalf("BuildSources: %v", err)
	}
	b, _ := BuildSources(testConfig())
	if len(a) != 3 || len(b) != 3 {
		t.Fatalf("source count = %d", len(a))
	}
	for i := range a {
		if a[i].Name != b[i].Name || len(a[i].Segments) != len(b[i].Segments) {
			t.Fatalf("layer %d mismatch", i)
		}
		for j := range a[i].Segments {
			if string(a[i].Segments[j].Bytes) != string(b[i].Segments[j].Bytes) {
				t.Fatalf("layer %d segment %d not reproducible", i, j)
			}
		}
	}
}

func TestBuildSourcesRejectsInvalid(t *testing.T) {
	cfg := testConfig()
	cfg.Representations = 99
	if _, err := BuildSources(cfg); err == nil {
		t.Fatal("expected validation error")
	}
}

func TestRunAcceptanceThresholds(t *testing.T) {
	s, err := Run(testConfig())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !s.Pass {
		t.Fatalf("summary failed: %v", s.Failures)
	}
	if s.SyncErrorMS > 40 {
		t.Fatalf("sync_error_ms = %v", s.SyncErrorMS)
	}
	if s.LossRecoveryRate < 0.90 || s.LossRecoveryRate > 1.00 {
		t.Fatalf("loss_recovery_rate = %v", s.LossRecoveryRate)
	}
	if s.LatencyP95MS > 100 {
		t.Fatalf("p95 = %v", s.LatencyP95MS)
	}
	if s.DamagedCount == 0 {
		t.Fatal("expected damaged ranges to be reported")
	}
	if len(s.GOPIntervalsMS) == 0 {
		t.Fatal("expected GOP intervals")
	}
	if s.BoundaryMismatchCount == 0 {
		t.Fatal("expected the planted cross-layer boundary mismatch")
	}
	if s.Truth == nil || len(s.Truth) != 3 {
		t.Fatalf("truth = %+v", s.Truth)
	}
}

func TestRunReproducibleAcrossSeeds(t *testing.T) {
	cfg := testConfig()
	a, err := Run(cfg)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	b, _ := Run(cfg)
	if a.DeterministicDigest != b.DeterministicDigest {
		t.Fatalf("digest drift: %s vs %s", a.DeterministicDigest, b.DeterministicDigest)
	}
	if len(a.DamagedRanges) != len(b.DamagedRanges) {
		t.Fatalf("damage range count drift: %d vs %d", len(a.DamagedRanges), len(b.DamagedRanges))
	}
	for i := range a.DamagedRanges {
		if a.DamagedRanges[i] != b.DamagedRanges[i] {
			t.Fatalf("damage range %d drift: %+v vs %+v", i, a.DamagedRanges[i], b.DamagedRanges[i])
		}
	}
	// A different seed must produce a different fixture/digest.
	other := cfg
	other.Seed = 99
	c, _ := Run(other)
	if c.DeterministicDigest == a.DeterministicDigest {
		t.Fatal("different seed produced identical digest")
	}
}

func TestRunInjectedDamageKindsPresent(t *testing.T) {
	s, err := Run(testConfig())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	kinds := make(map[string]int)
	for _, d := range s.DamagedRanges {
		kinds[d.Kind]++
	}
	for _, want := range []string{
		string(clock.ErrMfhdGap), string(clock.ErrMfhdDuplicate),
		string(clock.ErrTfdtRegression), string(clock.ErrSampleOutOfBounds),
	} {
		if kinds[want] == 0 {
			t.Fatalf("missing damage kind %q; have %v", want, kinds)
		}
	}
}

func TestLayerFailureIsolation(t *testing.T) {
	srcs, err := BuildSources(testConfig())
	if err != nil {
		t.Fatalf("BuildSources: %v", err)
	}
	cfg := testConfig()
	healthy := ProcessRepresentation(srcs[0], cfg)
	if healthy.Layer.Failed {
		t.Fatalf("layer 0 unexpectedly failed: %+v", healthy.Layer)
	}
	// Break only layer 0's init; layer 1 must be unaffected.
	broken := srcs[0]
	broken.Init = []byte("not a valid cmaf init")
	bad := ProcessRepresentation(broken, cfg)
	if !bad.Failed || bad.Layer.Failed == false {
		t.Fatalf("broken layer should fail: %+v", bad)
	}
	other := ProcessRepresentation(srcs[1], cfg)
	if other.Layer.Failed {
		t.Fatalf("neighbour layer must be unaffected: %+v", other.Layer)
	}
}

func TestLayerDigestsIndependent(t *testing.T) {
	srcs, _ := BuildSources(testConfig())
	cfg := testConfig()
	a := ProcessRepresentation(srcs[1], cfg)
	// Recover layer 1 twice: same digest regardless of layer 0's state.
	_ = ProcessRepresentation(srcs[0], cfg)
	b := ProcessRepresentation(srcs[1], cfg)
	if a.Layer.Digest != b.Layer.Digest {
		t.Fatalf("layer digest depends on another layer: %s vs %s", a.Layer.Digest, b.Layer.Digest)
	}
}

func TestSummaryJSONContract(t *testing.T) {
	s, err := Run(testConfig())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	required := []string{
		"damaged_ranges", "gop_interval_ms", "boundary_mismatch_count",
		"sync_error_ms", "loss_recovery_rate", "p95_latency_ms",
		"p50_latency_ms", "recovery_digest", "thresholds", "pass",
		"latency_scope", "layers",
	}
	for _, k := range required {
		if _, ok := m[k]; !ok {
			t.Fatalf("summary missing key %q", k)
		}
	}
	if !strings.Contains(string(b), "gop_interval_mean_ms") {
		t.Fatal("per-layer GOP stats missing")
	}
}

func TestBoundaryChecksReference(t *testing.T) {
	s, err := Run(testConfig())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if s.BoundaryRef == "" {
		t.Fatal("no reference layer selected")
	}
	var refCheck bool
	for _, c := range s.BoundaryChecks {
		if c.Layer == s.BoundaryRef {
			refCheck = true
			if c.StartDeltaMS != 0 || c.EndDeltaMS != 0 {
				t.Fatalf("reference layer has non-zero delta: %+v", c)
			}
		}
	}
	if !refCheck {
		t.Fatal("reference layer absent from checks")
	}
	// Every layer must appear exactly once.
	if len(s.BoundaryChecks) != len(s.Layers) {
		t.Fatalf("checks = %d layers = %d", len(s.BoundaryChecks), len(s.Layers))
	}
}

func TestCrossCheckToleranceConfigurable(t *testing.T) {
	cfg := testConfig()
	cfg.MismatchTolMS = 0
	s, err := Run(cfg)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if s.SyncErrorMS > cfg.SyncTolMS {
		t.Fatalf("sync error exceeds tolerance: %v", s.SyncErrorMS)
	}
	if s.LossRecoveryRate < s.Thresholds.MinRecoveryRate {
		t.Fatalf("recovery = %v", s.LossRecoveryRate)
	}
}

var _ = repair.DefaultOptions
