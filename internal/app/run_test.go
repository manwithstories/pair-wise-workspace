package app

import (
	"context"
	"strings"
	"testing"

	"mediacheck/internal/fixture"
	"mediacheck/internal/model"
	"mediacheck/internal/remux"
)

func baseOpts(profile fixture.Profile) Options {
	o := DefaultOptions()
	o.Profile = profile
	o.SlowSinkMs = 0 // keep tests fast; backpressure has its own test
	return o
}

func TestRunNormalProfileExitZeroAndMetrics(t *testing.T) {
	res, err := Run(context.Background(), baseOpts(fixture.ProfileNormal))
	if err != nil {
		t.Fatal(err)
	}
	if res.Exit != model.ExitOK {
		t.Fatalf("normal profile exit=%d want 0", res.Exit)
	}
	s := res.Summary
	if s.AVSyncMaxMs > 20 {
		t.Fatalf("av_sync_max_ms=%d exceeds 20", s.AVSyncMaxMs)
	}
	if s.LossRecoveryRate < 0.90 {
		t.Fatalf("loss_recovery_rate=%f below 0.90", s.LossRecoveryRate)
	}
	if s.LatencyP95Ms > 500 {
		t.Fatalf("latency_p95_ms=%d exceeds 500", s.LatencyP95Ms)
	}
	if s.PeakBufferMiB > 32 {
		t.Fatalf("peak_buffer_mib=%f exceeds 32", s.PeakBufferMiB)
	}
	if !strings.Contains(remux.Render(s), "fault_segments") {
		t.Fatal("summary must render documented fields")
	}
}

func TestRunDamageProfilesExitOne(t *testing.T) {
	for _, p := range []fixture.Profile{
		fixture.ProfileTruncated, fixture.ProfileCorrupt,
		fixture.ProfileNoKeyframe, fixture.ProfileDrift,
	} {
		res, err := Run(context.Background(), baseOpts(p))
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		if res.Exit != model.ExitDamaged {
			t.Fatalf("%s exit=%d want %d", p, res.Exit, model.ExitDamaged)
		}
		if len(res.Segments) == 0 {
			t.Fatalf("%s must report fault segments", p)
		}
	}
}

func TestRunCorruptIsolatesButKeepsLaterSegments(t *testing.T) {
	res, _ := Run(context.Background(), baseOpts(fixture.ProfileCorrupt))
	quarantined := 0
	var corruptSeg *model.Segment
	for i := range res.Segments {
		seg := &res.Segments[i]
		if seg.Recovery == model.RecQuarantined {
			quarantined++
			if seg.Reason == model.KindCorruptSample {
				corruptSeg = seg
			}
		}
	}
	if quarantined == 0 || corruptSeg == nil {
		t.Fatal("corrupt profile must quarantine the damaged span")
	}
	// A committed boundary must still reach the track end, proving the
	// pipeline kept processing valid content after the isolated hole.
	reachedEnd := false
	for _, b := range res.Committed {
		if b.EndMs > corruptSeg.EndMs {
			reachedEnd = true
		}
	}
	if !reachedEnd {
		t.Fatal("later valid content must remain processable after quarantine")
	}
}

func TestRunDeterministicAcrossRuns(t *testing.T) {
	first, err := Run(context.Background(), baseOpts(fixture.ProfileBroadcast))
	if err != nil {
		t.Fatal(err)
	}
	second, err := Run(context.Background(), baseOpts(fixture.ProfileBroadcast))
	if err != nil {
		t.Fatal(err)
	}
	if remux.Render(first.Summary) != remux.Render(second.Summary) {
		t.Fatal("summary differs across repeated runs")
	}
	if len(first.Segments) != len(second.Segments) {
		t.Fatal("segment count differs across repeated runs")
	}
	for i := range first.Segments {
		a, b := first.Segments[i], second.Segments[i]
		if a.StartMs != b.StartMs || a.EndMs != b.EndMs || a.Recovery != b.Recovery {
			t.Fatalf("segment %d differs across runs", i)
		}
	}
}

func TestRunRejectsOverMemoryInput(t *testing.T) {
	o := baseOpts(fixture.ProfileNormal)
	o.MemoryMiB = -1 // invalid budget
	res, err := Run(context.Background(), o)
	if err == nil || res.Exit != model.ExitInvalidArgs {
		t.Fatalf("invalid memory budget must return exit 2, got %v exit=%d", err, res.Exit)
	}
}

func TestRunBoundaryOrphanEvent(t *testing.T) {
	res, err := Run(context.Background(), baseOpts(fixture.ProfileBoundary))
	if err != nil {
		t.Fatal(err)
	}
	if res.Summary.OrphanEvents == 0 {
		t.Fatal("boundary profile must retain an orphan event")
	}
}
