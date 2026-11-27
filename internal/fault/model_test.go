package fault

import "testing"

func TestRecorderCountsAndRecovery(t *testing.T) {
	r := NewRecorder()
	r.Add(KindMissingSegment, "stage", "video", 3, 270000, true, "slot missing")
	r.Add(KindTruncatedPacket, "stage", "video", 7, 630000, false, "truncated")
	r.AddNoSeq(KindPathTraversal, "side", "manifest", false, "../escape")

	if r.Len() != 3 {
		t.Fatalf("len = %d, want 3", r.Len())
	}
	if r.CountRecoverable() != 1 || r.CountRecovered() != 0 {
		t.Fatalf("recoverable=%d recovered=%d", r.CountRecoverable(), r.CountRecovered())
	}
	if !r.HasUnrecoverable() {
		t.Fatal("expected unrecoverable faults present")
	}
	if n := r.MarkRecovered(KindMissingSegment, 3); n != 1 {
		t.Fatalf("marked %d, want 1", n)
	}
	if r.CountRecovered() != 1 {
		t.Fatalf("recovered = %d, want 1", r.CountRecovered())
	}
	counts := r.CountByKind()
	if counts[KindMissingSegment] != 1 || counts[KindPathTraversal] != 1 {
		t.Fatalf("counts = %v", counts)
	}
}

func TestNewPreservesZeroValues(t *testing.T) {
	// Sequence 0 and offset 0 are legitimate and must keep their presence flags.
	f := New(KindOutOfOrder, "cam", "video", true, 0, true, 0, true, "x")
	if !f.HasSequence || f.Sequence != 0 || !f.HasOffset || f.Offset90k != 0 {
		t.Fatalf("zero-value positioning lost: %+v", f)
	}
	g := New(KindPathTraversal, "cam", "track", false, 0, false, 0, false, "y")
	if g.HasSequence || g.Sequence != -1 || g.HasOffset || g.Offset90k != -1 {
		t.Fatalf("sentinel not applied: %+v", g)
	}
}
