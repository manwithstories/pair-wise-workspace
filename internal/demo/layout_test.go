package demo

import (
	"context"
	"testing"
)

func TestUnknownLayoutFallsBackExplicitly(t *testing.T) {
	res, err := Run(context.Background(), Options{
		Seed: 42, ReorderWindow: 64, Quiet: true, LayoutOverride: "unknown",
	})
	if err != nil {
		t.Fatal(err)
	}
	rep := res.Takes[0].Report
	if !rep.LayoutFallback {
		t.Fatal("unknown layout must be flagged as a fallback")
	}
	if rep.LayoutResolved != "stereo" {
		t.Fatalf("2-channel unknown layout should resolve stereo, got %q", rep.LayoutResolved)
	}
	if rep.LayoutReason == "" {
		t.Fatal("fallback reason must be recorded")
	}
	if rep.OutputLUFS < -17 || rep.OutputLUFS > -15 {
		t.Fatalf("fallback audio not normalised: %f LUFS", rep.OutputLUFS)
	}
	if rep.SyncErrorMSP95 > 20 {
		t.Fatalf("fallback broke sync: %f", rep.SyncErrorMSP95)
	}
}
