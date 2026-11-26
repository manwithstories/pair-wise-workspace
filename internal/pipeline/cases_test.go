package pipeline_test

import (
	"testing"

	"avpipe/internal/pipeline"
)

func runCase(t *testing.T, name string) *pipeline.Report {
	t.Helper()
	rep, err := pipeline.RunDemo(pipeline.Options{
		Out: t.TempDir(), Seed: 23, Case: name,
		Bitrates: []int64{600000, 1200000}, Segments: 12,
	})
	if err != nil {
		t.Fatalf("case %s: %v", name, err)
	}
	return rep
}

func TestIndividualFixtureCases(t *testing.T) {
	t.Run("pcr_wrap", func(t *testing.T) {
		r := runCase(t, "pcr_wrap")
		if r.Packets != 1920 || r.KeyframeFound != 24 || r.AlignedSlots != 12 {
			t.Fatalf("metrics packets=%d key=%d/%d aligned=%d", r.Packets, r.KeyframeFound, r.KeyframeExpected, r.AlignedSlots)
		}
		wraps := 0
		for _, epochs := range r.ClockEpochs {
			for _, e := range epochs {
				wraps += e.PCRWraps
			}
		}
		if wraps == 0 {
			t.Fatal("PCR wrap was not expanded into epoch wrap counters")
		}
	})
	t.Run("cc_jump", func(t *testing.T) {
		r := runCase(t, "cc_jump")
		if r.CCGap != 1 || r.GapSlots != 1 || r.KeyframeFound != 24 {
			t.Fatalf("cc=%d gap=%d key=%d/%d", r.CCGap, r.GapSlots, r.KeyframeFound, r.KeyframeExpected)
		}
	})
	t.Run("psi_bad", func(t *testing.T) {
		r := runCase(t, "psi_bad")
		if r.PSICRCReject != 1 || r.GapSlots != 0 {
			t.Fatalf("psi=%d gap=%d", r.PSICRCReject, r.GapSlots)
		}
	})
	t.Run("missing_idr", func(t *testing.T) {
		r := runCase(t, "missing_idr")
		if r.KeyframeFound != 23 || r.KeyframeExpected != 24 || r.GapSlots != 1 {
			t.Fatalf("key=%d/%d gap=%d", r.KeyframeFound, r.KeyframeExpected, r.GapSlots)
		}
	})
}
