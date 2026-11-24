package media

import (
	"errors"
	"math"
	"testing"
)

func TestConvertTicksAudioToShared(t *testing.T) {
	// One 48 kHz audio frame interval of 1600 ticks is 33.333... ms,
	// represented as 3000 ticks in the shared 90 kHz base.
	got, err := ConvertTicks(1600, TrackTimebase(48000), Timebase())
	if err != nil {
		t.Fatal(err)
	}
	if got != 3000 {
		t.Fatalf("ticks=%d want 3000", got)
	}
}

func TestConvertTicksOverflowIsLocatedAsValueError(t *testing.T) {
	_, err := ConvertTicks(math.MaxInt64, Rational{Num: math.MaxInt64, Den: 1}, Timebase())
	if err == nil {
		t.Fatal("expected overflow")
	}
}

func TestErrorOriginPreservesFragmentAndOffset(t *testing.T) {
	root := Corrupt("clock", "bad_dts", "seg-9.m4s", 9, 12345, "boom")
	wrapped := Wrap(root, "pipeline")
	var me *Error
	if !errors.As(wrapped, &me) {
		t.Fatal("shared error not in chain")
	}
	fragment, offset, path := Origin(wrapped)
	if fragment != 9 || offset != 12345 || path != "seg-9.m4s" {
		t.Fatalf("origin=(%d,%d,%q), want 9,12345,seg-9.m4s", fragment, offset, path)
	}
	if ExitCodeOf(wrapped) != ExitCorrupt {
		t.Fatalf("exit=%d want 2", ExitCodeOf(wrapped))
	}
}
