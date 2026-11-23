package mp4

import (
	"errors"
	"testing"
)

func TestCrossValidateAgrees(t *testing.T) {
	data := BuildFixture(FixtureSpec{
		Tracks: 3, Frames: 12, Timescale: 30_000, KeyEvery: 4,
	})
	roots, err := ScanBoxes(data, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	counts, err := CrossValidate(data, roots)
	if err != nil {
		t.Fatalf("cross validation: %v", err)
	}
	if counts.Traks != 3 {
		t.Fatalf("traks=%d want 3", counts.Traks)
	}
	if counts.Stts != 3 || counts.Stsz != 3 || counts.Stco != 3 {
		t.Fatalf("table counts wrong: %+v", counts)
	}
	if counts.Mdats != 1 || counts.Total <= 0 {
		t.Fatalf("mdat/total wrong: %+v", counts)
	}
}

func TestCrossValidateDetectsTruncation(t *testing.T) {
	data := BuildFixture(FixtureSpec{
		Tracks: 1, Frames: 8, Timescale: 30_000,
		KeyEvery: 2, CorruptAtFrame: 3, Corrupt: true,
	})
	roots, err := ScanBoxes(data, DefaultLimits())
	if !errors.Is(err, ErrTruncated) {
		t.Fatalf("in-tree scanner err=%v want truncation", err)
	}
	// The external library must independently reject the same truncation.
	if _, err := CrossValidate(data, roots); err == nil {
		t.Fatal("go-mp4 should reject the truncated stream")
	}
}

func TestCrossValidateOffsetExactness(t *testing.T) {
	data := BuildFixture(FixtureSpec{
		Tracks: 2, Frames: 6, Timescale: 90_000, KeyEvery: 3,
	})
	roots, _ := ScanBoxes(data, DefaultLimits())
	counts, err := CrossValidate(data, roots)
	if err != nil {
		t.Fatalf("offset cross-check: %v", err)
	}
	// stss present because KeyEvery>0; co64 absent (32-bit stco used).
	if counts.Stss != 2 || counts.Co64 != 0 {
		t.Fatalf("stss/co64 counts wrong: %+v", counts)
	}
}
