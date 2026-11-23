package metadata

import (
	"errors"
	"testing"

	"mediascan/internal/clock"
	"mediascan/internal/fault"
)

func TestNormalizeAllKinds(t *testing.T) {
	raws := []RawEvent{
		{Kind: KindSubtitle, ID: "sub1", TimeUnits: 0, DurationUnits: 1000, Payload: "hi"},
		{Kind: KindChapter, ID: "ch1", TimeUnits: 3000, DurationUnits: 0},
		{Kind: KindID3, ID: "id3", TimeUnits: 6000},
		{Kind: KindEmsg, ID: "emsg", TimeUnits: 9000},
		{Kind: KindSEI, ID: "sei", TimeUnits: 12000},
	}
	evs, err := NormalizeEvents(7, 30_000, raws)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 5 || evs[0].TrackID != 7 {
		t.Fatalf("normalization wrong: %+v", evs)
	}
	// 1000 units @30k = 33333 ticks exactly (1e6*1000/30000 truncated).
	if evs[0].StartTicks != 0 || evs[0].DurationTicks != 33_333 {
		t.Fatalf("tick mapping wrong: %+v", evs[0])
	}
	// 9000 units = 300000 ticks.
	if evs[3].StartTicks != 300_000 {
		t.Fatalf("emsg ticks=%d want 300000", evs[3].StartTicks)
	}
}

func TestNegativeDurationRejected(t *testing.T) {
	_, err := NormalizeEvents(1, 90_000, []RawEvent{
		{Kind: KindSEI, TimeUnits: 0, DurationUnits: -5},
	})
	if !errors.Is(err, clock.ErrNegativeDuration) {
		t.Fatalf("err=%v want negative duration", err)
	}
}

func TestBadTimescale(t *testing.T) {
	if _, err := NormalizeEvents(1, 0, []RawEvent{{Kind: KindID3, TimeUnits: 1}}); err == nil {
		t.Fatal("expected invalid timescale error")
	}
	if _, err := NormalizeEvents(1, 30_000, nil); !errors.Is(err, ErrNoEvents) {
		t.Fatalf("empty err=%v", err)
	}
}

func TestDriftAccumulationVFR(t *testing.T) {
	// Events on a VFR-style timeline; observed positions accumulate a
	// growing positive drift (ahead) while reference stays linear.
	events := []Event{
		{Kind: KindEmsg, TrackID: 3, ID: "e0", StartTicks: 100_000, RefTicks: 100_000},
		{Kind: KindEmsg, TrackID: 3, ID: "e1", StartTicks: 200_500, RefTicks: 200_000},
		{Kind: KindEmsg, TrackID: 3, ID: "e2", StartTicks: 301_500, RefTicks: 300_000},
		{Kind: KindEmsg, TrackID: 3, ID: "e3", StartTicks: 403_000, RefTicks: 400_000},
	}
	const threshold = int64(1_000)
	rep, fs := DetectDrift(events, threshold)
	// First exceedance is event e2 (deviation 1500); e1 (500) stays under.
	if rep.FirstExceedance == nil || rep.FirstExceedance.EventIndex != 2 {
		t.Fatalf("first exceedance wrong: %+v", rep.FirstExceedance)
	}
	if rep.FirstExceedance.Direction != DriftAhead {
		t.Fatalf("direction=%s want ahead", rep.FirstExceedance.Direction)
	}
	if rep.MaxAbsDeviation != 3_000 {
		t.Fatalf("max deviation=%d want 3000", rep.MaxAbsDeviation)
	}
	if len(fs) != 1 || fs[0].Kind != fault.KindClockDrift || fs[0].TrackID != 3 {
		t.Fatalf("drift fault wrong: %+v", fs)
	}
}

func TestDriftBehind(t *testing.T) {
	events := []Event{
		{Kind: KindSEI, TrackID: 9, ID: "a", StartTicks: 100, RefTicks: 5_000},
	}
	rep, fs := DetectDrift(events, 1_000)
	if rep.FirstExceedance == nil || rep.FirstExceedance.Direction != DriftBehind {
		t.Fatalf("behind drift not detected: %+v", rep.FirstExceedance)
	}
	if fs[0].FrameIndex != 0 {
		t.Fatalf("fault position wrong: %+v", fs[0])
	}
}

func TestDriftWithinThresholdClean(t *testing.T) {
	events := []Event{
		{Kind: KindSubtitle, TrackID: 1, ID: "x", StartTicks: 100, RefTicks: 0},
		{Kind: KindSubtitle, TrackID: 1, ID: "y", StartTicks: 600, RefTicks: 0},
	}
	rep, fs := DetectDrift(events, 5_000)
	if rep.FirstExceedance != nil || len(fs) != 0 {
		t.Fatalf("expected no drift fault: %+v %+v", rep, fs)
	}
}

func TestWithReferenceAlignment(t *testing.T) {
	evs := []Event{{ID: "a"}, {ID: "b"}}
	if _, err := WithReference(evs, []int64{1}); err == nil {
		t.Fatal("length mismatch must error")
	}
	out, err := WithReference(evs, []int64{10, 20})
	if err != nil {
		t.Fatal(err)
	}
	if out[0].RefTicks != 10 || out[1].RefTicks != 20 || evs[0].RefTicks != 0 {
		t.Fatal("reference copy must not mutate source")
	}
}
