package demo_test

import (
	"testing"

	"medialab/internal/demo"
	"medialab/internal/model"
)

func TestProvenancePreservesOffsetsStatesAndMetadataFault(t *testing.T) {
	r, err := demo.Run(demo.Options{Seed: 17, BurstLoss: 7, BandwidthKbps: []int{900, 280, 1200}})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Provenance) != 13 {
		t.Fatalf("provenance refs=%d, want 12 segments + rejected metadata", len(r.Provenance))
	}
	recovered := 0
	for _, ref := range r.Provenance[:12] {
		if ref.SourceOff < 0 {
			t.Fatalf("negative source offset: %+v", ref)
		}
		switch ref.State {
		case model.StateRetransmitted, model.StateXORRecovered:
			recovered++
		}
	}
	if recovered != 7 {
		t.Fatalf("recovered provenance states=%d, want 7", recovered)
	}
	last := r.Provenance[12]
	if last.State != model.StateRejected || last.SourceOff != r.CropOffset || last.OriginError == nil {
		t.Fatalf("metadata provenance=%+v cropOffset=%d", last, r.CropOffset)
	}
	if last.OriginError.Code != "crop_out_of_bounds" {
		t.Fatalf("origin code=%q", last.OriginError.Code)
	}
}
