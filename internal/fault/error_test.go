package fault

import (
	"errors"
	"testing"
)

func TestErrorCarriesOffsetAndTrack(t *testing.T) {
	err := AtElement(StageParse, 1234, "Segment", ErrTruncated).OnTrack(3)
	want := "stage=parse element=Segment track=3 offset=1234: truncated input"
	if err.Error() != want {
		t.Fatalf("got %q want %q", err.Error(), want)
	}
	if OffsetOf(err) != 1234 || TrackOf(err) != 3 || StageOf(err) != StageParse {
		t.Fatalf("accessors failed: %v %v %v", OffsetOf(err), TrackOf(err), StageOf(err))
	}
	if !errors.Is(err, ErrTruncated) {
		t.Fatalf("errors.Is lost the sentinel")
	}
}

func TestExitCodes(t *testing.T) {
	cases := []struct {
		err  error
		code int
	}{
		{nil, 0},
		{At(StageParse, 10, ErrTruncated), 2},
		{At(StageMap, -1, ErrUnsupportedCodec), 2},
		{New(StageIO, ErrOutput), 3},
		{New(StageVerify, ErrBoundary), 2},
		{errors.New("plain"), 3},
	}
	for _, c := range cases {
		if got := ExitCode(c.err); got != c.code {
			t.Errorf("ExitCode(%v)=%d want %d", c.err, got, c.code)
		}
	}
}

func TestIsCorrupt(t *testing.T) {
	if !IsCorrupt(New(StageDemux, ErrCorruptBlock)) {
		t.Fatal("corrupt block should be corrupt")
	}
	if IsCorrupt(New(StageQueue, ErrCancelled)) {
		t.Fatal("cancellation is not corruption")
	}
}

func TestRecoveryError(t *testing.T) {
	re := &RecoveryError{
		Cause: At(StageParse, 100, ErrTruncated),
		Skips: []Skip{{Start: 100, Length: 12, Reason: "cluster boundary"}, {Start: 200, Length: 8}},
	}
	if re.SkippedBytes() != 20 {
		t.Fatalf("SkippedBytes=%d", re.SkippedBytes())
	}
	if !errors.Is(re, ErrTruncated) {
		t.Fatal("RecoveryError must unwrap to its cause")
	}
	if ExitCode(re) != 2 {
		t.Fatalf("exit=%d", ExitCode(re))
	}
}
