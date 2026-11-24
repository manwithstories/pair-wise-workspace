package source_test

import (
	"path/filepath"
	"testing"

	"mediad/internal/fixture"
	"mediad/internal/source"
)

func cleanRoot(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	cfg := fixture.CleanConfig(filepath.Join(dir, "media"), 23)
	if err := fixture.Generate(cfg); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, "media")
}

func TestDiscoverClassifiesFragmentsAndStableDigest(t *testing.T) {
	root := cleanRoot(t)
	plan, err := source.Discover(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Fragments) != 24 {
		t.Fatalf("fragments=%d want 24", len(plan.Fragments))
	}
	if len(plan.Inits) != 3 {
		t.Fatalf("inits=%d want 3", len(plan.Inits))
	}
	if len(plan.RTP) != 24 {
		t.Fatalf("rtp sidecars=%d want 24", len(plan.RTP))
	}
	again, err := source.Discover(root)
	if err != nil {
		t.Fatal(err)
	}
	if again.InputDigest != plan.InputDigest {
		t.Fatal("input digest must be stable across discovery runs")
	}
	if plan.InputDigest == "" {
		t.Fatal("input digest must not be empty")
	}
}

func TestAnalyzeOneParsesRoundTripsAndRTP(t *testing.T) {
	root := cleanRoot(t)
	plan, err := source.Discover(root)
	if err != nil {
		t.Fatal(err)
	}
	first := plan.Fragments[0] // sorted: cam01/high/seg_00001.m4s
	side := plan.RTP[first.Rel]
	oc := source.AnalyzeOne(source.Job{
		Index: 0, Src: first, Timescale: 90000, RTPClock: 90000, RTPSidecar: side.Abs,
	})
	if oc.Fault != nil {
		t.Fatalf("fault: %v", oc.Fault)
	}
	if !oc.Parsed || !oc.RoundTripOK {
		t.Fatalf("parsed=%v roundtrip=%v want both true", oc.Parsed, oc.RoundTripOK)
	}
	if oc.Observation.Sequence != 1 || len(oc.Observation.Samples) == 0 {
		t.Fatalf("bad observation: seq=%d samples=%d", oc.Observation.Sequence, len(oc.Observation.Samples))
	}
	if !oc.Observation.RTP.Present || oc.Observation.RTP.Timestamp != 0 {
		t.Fatalf("rtp evidence wrong: %+v", oc.Observation.RTP)
	}
	if oc.Src.Representation != "high" || oc.Src.Camera != "cam01" {
		t.Fatalf("classification wrong: %s/%s", oc.Src.Camera, oc.Src.Representation)
	}
}

func TestDiscoverRejectsMissingRoot(t *testing.T) {
	if _, err := source.Discover(filepath.Join(t.TempDir(), "does-not-exist")); err == nil {
		t.Fatal("missing root must return an error")
	}
}
