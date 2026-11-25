package fieldpost_test

import (
	"context"
	"testing"

	"fieldpost/internal/demo"
)

func TestRootPressurePipelineHonoursCap(t *testing.T) {
	snap, err := demo.RunPressure(context.Background(), 2<<20)
	if err != nil {
		t.Fatal(err)
	}
	if snap.BackpressureEvents == 0 {
		t.Fatal("expected backpressure")
	}
	if snap.PeakInFlight > 2<<20 {
		t.Fatalf("peak=%d cap=%d", snap.PeakInFlight, 2<<20)
	}
	if snap.InFlight != 0 {
		t.Fatalf("inflight=%d", snap.InFlight)
	}
}
