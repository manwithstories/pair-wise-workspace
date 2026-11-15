package state

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestJournalReplayAndBadLineWarning(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.log")
	j, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	result := SegmentResult{Index: 0, Frames: 4, TargetKbps: 500, ActualKbps: 495, DevPct: -1,
		PSNRDB: 35, Status: Done}
	if err := j.Append(Record{Type: RunStarted, PlanID: "plan-a", Segment: -1}); err != nil {
		t.Fatal(err)
	}
	if err := j.Append(Record{Type: SegmentDone, PlanID: "plan-a", Segment: 0, Result: &result}); err != nil {
		t.Fatal(err)
	}
	if err := j.WriteCheckpoint("plan-a", []SegmentResult{result}); err != nil {
		t.Fatal(err)
	}
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("{not-json}\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	snapshot, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Warnings) != 1 || !strings.Contains(snapshot.Warnings[0], "line 4") {
		t.Fatalf("expected one bad-line warning, got %#v", snapshot.Warnings)
	}
	if got, ok := snapshot.Completed[0]; !ok || got.PSNRDB != 35 {
		t.Fatalf("completed result not replayed: %#v", snapshot.Completed)
	}
	if err := snapshot.ValidatePlan("plan-a", 2); err != nil {
		t.Fatalf("matching plan rejected: %v", err)
	}
	if err := snapshot.ValidatePlan("plan-b", 2); !errors.Is(err, ErrPlanMismatch) {
		t.Fatalf("expected mismatch, got %v", err)
	}
	if err := snapshot.ValidateSegments("plan-a", []ExpectedSegment{{Frames: 4, TargetKbps: 500}}); err != nil {
		t.Fatalf("matching segment rejected: %v", err)
	}
	if err := snapshot.ValidateSegments("plan-a", []ExpectedSegment{{Frames: 4, TargetKbps: 600}}); !errors.Is(err, ErrPlanMismatch) {
		t.Fatalf("expected semantic mismatch, got %v", err)
	}
}

func TestCheckpointReplaysCompletedResults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.log")
	j, _ := Open(path)
	result := SegmentResult{Index: 1, Frames: 2, TargetKbps: 100, ActualKbps: 100, Status: Done}
	if err := j.WriteCheckpoint("p", []SegmentResult{result}); err != nil {
		t.Fatal(err)
	}
	j.Close()
	snapshot, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := snapshot.Completed[1]; !ok || len(snapshot.Checkpoint) != 1 {
		t.Fatalf("checkpoint not replayed: %#v", snapshot)
	}
}
