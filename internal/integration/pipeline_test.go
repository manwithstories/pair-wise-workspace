package integration

import (
	"context"
	"testing"
	"time"

	"relay/internal/fixture"
	"relay/internal/ingest"
	"relay/internal/media"
	"relay/internal/output"
	"relay/internal/stream"
	"relay/internal/timeline"
)

func healthyReports(t *testing.T, sc *fixture.Scenario) []*timeline.TrackReport {
	t.Helper()
	byTrack := map[uint32][]*ingest.Fragment{}
	for _, in := range sc.Files {
		if in.Truncated {
			continue
		}
		f, fault := ingestOne(t, in)
		if fault != nil {
			t.Fatalf("unexpected fault: %v", fault)
		}
		byTrack[f.TrackID] = append(byTrack[f.TrackID], f)
	}
	return []*timeline.TrackReport{
		timeline.RepairTrack(101, media.Clock{TrackID: 101, Timescale: 90000}, byTrack[101]),
		timeline.RepairTrack(201, media.Clock{TrackID: 201, Timescale: 48000}, byTrack[201]),
		timeline.RepairTrack(301, media.Clock{TrackID: 301, Timescale: 48000}, byTrack[301]),
	}
}

// TestSlowConsumerBackPressureAndResume exercises a deliberately slow
// consumer under a tight queue budget, interrupts the run and confirms the
// resume produces a complete, bounded result without ever exceeding the
// configured queue-bytes ceiling.
func TestSlowConsumerBackPressureAndResume(t *testing.T) {
	sc := fixture.BuildAllScenario()
	reports := healthyReports(t, sc)
	const budget int64 = 4096 // far smaller than the whole working set
	dir := t.TempDir()

	first, err := stream.New(stream.Config{
		OutDir: dir, QueueBytes: budget, ConsumerDelay: 2 * time.Millisecond,
		InterruptAfter: 9,
	}, output.NewWriter())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := first.Run(ctx, reports); err != stream.ErrInterrupted {
		t.Fatalf("want interrupt, got %v", err)
	}
	if peak := first.PeakQueueBytes(); peak > budget {
		t.Fatalf("queue peak %d exceeded budget %d under back pressure", peak, budget)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	second, err := stream.New(stream.Config{
		OutDir: dir, QueueBytes: budget, ConsumerDelay: 2 * time.Millisecond,
	}, output.NewWriter())
	if err != nil {
		t.Fatal(err)
	}
	c, err := second.Run(ctx, reports)
	if err != nil {
		t.Fatalf("resume failed: %v", err)
	}
	if c.Skipped != 9 {
		t.Fatalf("resume must skip the 9 committed fragments, skipped=%d", c.Skipped)
	}
	if peak := second.PeakQueueBytes(); peak > budget {
		t.Fatalf("resume queue peak %d exceeded budget %d", peak, budget)
	}

	wantAccepted := 0
	for _, r := range reports {
		wantAccepted += len(r.Accepted)
	}
	if got := len(second.Checkpoint().Entries()); got != wantAccepted {
		t.Fatalf("after resume checkpoint has %d entries, want %d", got, wantAccepted)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
}
