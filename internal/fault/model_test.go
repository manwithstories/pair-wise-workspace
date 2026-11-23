package fault

import (
	"errors"
	"fmt"
	"testing"
)

func TestFaultFieldsAndError(t *testing.T) {
	f := New(KindCorruptSample, SkipFragment, 1234, "bad byte").WithTrack(7, 2)
	if !f.Recoverable() || f.TrackID != 7 || f.TrackIndex != 2 || f.Offset != 1234 {
		t.Fatalf("unexpected fault: %+v", f)
	}
	if s := f.Error(); s == "" || fmt.Sprint(s) == "" {
		t.Fatal("error string empty")
	}
	a := New(KindTruncatedHeader, Abort, 10, "eof")
	if a.Recoverable() {
		t.Fatal("abort fault must not be recoverable")
	}
}

func TestAsAndUnwrap(t *testing.T) {
	orig := New(KindMissingIndex, Abort, 99, "no stsz")
	wrapped := fmt.Errorf("wrap: %w", orig)
	if got := As(wrapped); got != orig {
		t.Fatal("As failed through wrapped error")
	}
	if As(errors.New("plain")) != nil {
		t.Fatal("plain error must not map to a fault")
	}
}

func TestFirst(t *testing.T) {
	late := New(KindCorruptSample, SkipFragment, 500, "x")
	early := New(KindTruncatedHeader, Abort, 50, "y")
	if got := First([]*Fault{late, nil, early}); got != early {
		t.Fatal("First must pick smallest offset")
	}
	if First(nil) != nil {
		t.Fatal("empty First must be nil")
	}
}
