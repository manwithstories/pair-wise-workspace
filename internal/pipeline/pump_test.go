package pipeline

import (
	"context"
	"testing"
)

func TestPoliciesDivergeUnderBacklog(t *testing.T) {
	// A consumer that takes longer than the block cadence builds a backlog
	// once the queue is small: block waits, drop discards, drop-oldest
	// evicts. All three must stay inside the byte limit.
	base := Options{Seed: 5, ConsumerDelayMs: 30, QueueBytes: 4096, QueueTicks: 3600}
	for _, policy := range []string{"block", "drop", "drop-oldest"} {
		o := base
		o.Policy = policy
		rep := Run(context.Background(), o)
		if rep.RootError != nil {
			t.Fatalf("%s root=%+v", policy, rep.RootError)
		}
		if rep.QueuePeakBytes > o.QueueBytes {
			t.Fatalf("%s peak %d exceeds %d", policy, rep.QueuePeakBytes, o.QueueBytes)
		}
		// Under real backlog the drop policies must discard work and the
		// block policy must not; all three stay inside the byte limit.
		switch policy {
		case "drop", "drop-oldest":
			if rep.Dropped+rep.Evicted == 0 {
				t.Fatalf("%s neither dropped nor evicted (blocks=%d)", policy, rep.Blocks)
			}
		case "block":
			if rep.Dropped+rep.Evicted != 0 {
				t.Fatalf("block policy dropped %d / evicted %d", rep.Dropped, rep.Evicted)
			}
			if rep.Blocks != rep.LatencySamples {
				t.Fatalf("block policy delivered %d blocks but measured %d", rep.Blocks, rep.LatencySamples)
			}
		}
	}
}
