package engine_test

import (
	"path/filepath"
	"testing"

	"mediaroute/internal/engine"
	"mediaroute/internal/fault"
	"mediaroute/internal/fixture"
)

// TestWraparoundLossPropagatesToSampleDamage covers the second fault
// class: a 16-bit sequence wraparound combined with a burst loss, proving
// packet-layer damage still maps to samples across the wrap boundary.
func TestWraparoundLossPropagatesToSampleDamage(t *testing.T) {
	spec := fixture.DefaultSpec(41)
	spec.DropBurstAt = 65532
	spec.DropBurstN = 8
	h := newHarness(t, spec)
	input := h.input(t)
	rep := scan(t, input, filepath.Join(h.dir, "idx"))
	cfg := engine.StreamConfig{
		Spec: spec, Report: rep,
		OutputPath:    filepath.Join(h.dir, "wrap.out"),
		Checkpoint:    filepath.Join(h.dir, "wrap.cp"),
		StartSeq:      65530,
		GapTimeoutNs:  50_000_000,
		BandwidthMbps: []float64{6, 1, 9},
	}
	st, err := engine.Stream(cfg)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if len(st.Gaps) == 0 {
		t.Fatal("wraparound burst produced no gaps")
	}
	if len(st.DamagedSamples) == 0 {
		t.Fatal("wraparound packet loss did not reach sample damage")
	}
	g := st.Gaps[0]
	if g.ExtLast <= g.ExtFirst {
		t.Fatalf("gap ext ordering wrong: %d..%d", g.ExtFirst, g.ExtLast)
	}
	if g.GapFault().Category != fault.CatPacketGap {
		t.Fatal("gap did not carry packet_gap category")
	}
}

// TestExitCodes verifies the documented process exit-code contract.
func TestExitCodes(t *testing.T) {
	cases := map[fault.Category]int{
		fault.CatBadArgument:          4,
		fault.CatCheckpointMismatch:   3,
		fault.CatIndexInvalid:         3,
		fault.CatTruncatedSampleTable: 2,
		fault.CatPacketGap:            2,
	}
	for cat, want := range cases {
		if got := fault.ExitCode(fault.New(cat, "x")); got != want {
			t.Fatalf("category %s -> exit %d want %d", cat, got, want)
		}
	}
}
