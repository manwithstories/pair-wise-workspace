package model

import (
	"errors"
	"testing"
)

func TestRecoverability(t *testing.T) {
	if CodeCheckpointMismatch.Recoverable() {
		t.Fatal("checkpoint mismatch must not be recoverable")
	}
	for _, c := range []ErrorCode{CodeContainerTruncated, CodeRTPUnrecoverable, CodeCanceled} {
		if !c.Recoverable() {
			t.Errorf("%s expected recoverable", c)
		}
	}
}

func TestMediaErrorIs(t *testing.T) {
	err := NewError(CodeCorruptSample, "master.mp4", 10, 20, "bad sample")
	sentinel := &MediaError{Code: CodeCorruptSample}
	if !errors.Is(err, sentinel) {
		t.Fatal("errors.Is should match by code")
	}
	if errors.Is(err, &MediaError{Code: CodeIndexMissingPage}) {
		t.Fatal("different codes must not match")
	}
}

func TestTimeRange(t *testing.T) {
	r := TimeRange{Start: 10, End: 20}
	if !r.Contains(10) || r.Contains(20) || r.Length() != 10 {
		t.Fatal("half-open semantics broken")
	}
	if r.Overlaps(TimeRange{Start: 20, End: 30}) {
		t.Fatal("touching ranges must not overlap")
	}
	if !r.Overlaps(TimeRange{Start: 15, End: 25}) {
		t.Fatal("overlapping range not detected")
	}
}
