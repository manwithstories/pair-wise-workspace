package pipeline

import (
	"strings"
	"testing"

	"mediarun/internal/model"
)

func TestSummarizeGaps(t *testing.T) {
	gaps := []model.Gap{
		{TrackID: 2, From: 10, To: 12, Count: 3},
		{TrackID: 1, From: 5, To: 5, Count: 1},
		{TrackID: 2, From: 20, To: 21, Count: 2},
	}
	got := summarizeGaps(gaps)
	if !strings.Contains(got, "gaps=3") {
		t.Fatalf("summary %q", got)
	}
	if !strings.Contains(got, "track-1:1") || !strings.Contains(got, "track-2:5") {
		t.Fatalf("per-track totals %q", got)
	}
	if summarizeGaps(nil) != "none" {
		t.Fatalf("empty summary %q", summarizeGaps(nil))
	}
}

func TestRoundTripRateDetectsChange(t *testing.T) {
	// An empty event set cannot have a round-trip rate.
	if rate := compareDigests(nil, nil, false); rate != 0 {
		t.Fatalf("empty rate %f", rate)
	}
	// Identical digests are a perfect rate.
	same := []string{"a|1", "b|2", "c|3"}
	if rate := compareDigests(same, same, false); rate != 1 {
		t.Fatalf("identical rate %f", rate)
	}
	// One changed digest out of four is 0.75.
	before := []string{"a|1", "b|2", "c|3", "d|4"}
	after := []string{"a|1", "b|2", "c|3", "d|9"}
	if rate := compareDigests(before, after, false); rate != 0.75 {
		t.Fatalf("partial rate %f", rate)
	}
	// A byte-level mismatch always forces zero.
	if rate := compareDigests(before, before, true); rate != 0 {
		t.Fatalf("byte mismatch rate %f", rate)
	}
}
