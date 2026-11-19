package pipeline

import (
	"testing"

	"pairflow/internal/rtp"
	"pairflow/internal/testkit"
)

func TestDbgReorder(t *testing.T) {
	fx := testkit.GenerateFixture(41, "changing")
	for _, cap := range fx.Captures {
		cfg := rtp.DefaultConfig()
		cfg.MaxCacheBytes = 256 * 1024 * 1024
		r := rtp.NewReorderer(cap.Spec.DeviceID, cfg)
		var out int
		for _, rec := range cap.Records {
			pkts, _, err := r.Push(rec)
			if err != nil {
				t.Fatalf("%s push: %v", cap.Spec.DeviceID, err)
			}
			out += len(pkts)
		}
		rest, gaps := r.Flush()
		out += len(rest)
		st := r.Stats()
		t.Logf("%s in=%d out=%d dup=%d conf=%d reord=%d gaps=%d total=%d",
			cap.Spec.DeviceID, len(cap.Records), out,
			st.Duplicates, st.Conflicts, st.Reordered, len(gaps), st.Total)
		for _, g := range gaps {
			t.Logf("  gap %d..%d missing=%d est=%d", g.StartSeq, g.EndSeq, g.MissingPackets, g.EstSamples)
		}
	}
}
