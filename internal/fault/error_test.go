package fault

import (
	"fmt"
	"testing"
)

func TestRecordCollectsAndClassifies(t *testing.T) {
	r := NewRecord().
		Add(New("rtp_gap", "audio", 17, LevelRecoverable, "one missing packet")).
		Add(New("trun_truncated", "video", 4096, LevelFatal, "sample count exceeds box"))
	if len(r.Faults) != 2 {
		t.Fatalf("got %d faults", len(r.Faults))
	}
	if len(r.OfLevel(LevelRecoverable)) != 1 {
		t.Fatal("expected one recoverable fault")
	}
	if !r.HasFatal() {
		t.Fatal("expected fatal classification")
	}
	if ExitCode(r) != 3 {
		t.Fatalf("fatal record exit=%d want 3", ExitCode(r))
	}
	if ExitCode(nil) != 0 {
		t.Fatal("nil error must map to 0")
	}
}

func TestAsRecordThroughWrap(t *testing.T) {
	rec := NewRecord().Add(New("x", "", 1, LevelAmbiguous, ""))
	wrapped := fmt.Errorf("scan failed: %w", rec)
	got, ok := AsRecord(wrapped)
	if !ok || len(got.Faults) != 1 {
		t.Fatalf("AsRecord failed: ok=%v", ok)
	}
}
