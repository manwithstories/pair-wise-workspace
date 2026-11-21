package txn

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"mediamerge/internal/demux"
	"mediamerge/internal/fixture"
	"mediamerge/internal/mapping"
)

// TestResidentSamplesStayBounded proves the streaming contract: with a queue
// of one, the pipeline still completes and never needs more than queue+2
// samples resident. QueuePeak is measured by the pipe itself.
func TestResidentSamplesStayBounded(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "in.mp4")
	plan := fixture.DefaultPlan(2, 60)
	if err := os.WriteFile(in, plan.Build(), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := demux.Open(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	mp, err := mapping.Apply(r.Tracks, mapping.DefaultRules(r.Tracks))
	if err != nil {
		t.Fatal(err)
	}
	res, err := Run(context.Background(), dir, r, mp, Options{Queue: 1, InterruptAfter: -1})
	if err != nil {
		t.Fatal(err)
	}
	if res.QueuePeak > 1+2 {
		t.Fatalf("queue peak %d exceeds capacity+2", res.QueuePeak)
	}
	if !res.SampleMatch || res.Samples != 120 {
		t.Fatalf("res=%+v", res)
	}
}
