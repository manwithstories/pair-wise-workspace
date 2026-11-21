package clock

import (
	"errors"
	"testing"
)

func TestBlockAccessors(t *testing.T) {
	b := Block{Channels: 2, Samples: make([]int32, 20), StartTick: 10}
	if b.Frames() != 10 {
		t.Fatalf("Frames() = %d, want 10", b.Frames())
	}
	if b.EndTick() != 20 {
		t.Fatalf("EndTick() = %d, want 20", b.EndTick())
	}
	if b.ByteSize() != 80 {
		t.Fatalf("ByteSize() = %d, want 80", b.ByteSize())
	}
	var zero Block
	if zero.Frames() != 0 || zero.EndTick() != 0 {
		t.Fatal("zero-value block must report zero extent")
	}
}

func TestErrorClassification(t *testing.T) {
	err := NewErrorAt(KindTruncatedWAV, 42, "header ends early")
	var ce *Error
	if !errors.As(err, &ce) {
		t.Fatal("error must satisfy errors.As to *clock.Error")
	}
	if ce.Kind != KindTruncatedWAV || ce.Offset != 42 {
		t.Fatalf("got kind=%s offset=%d", ce.Kind, ce.Offset)
	}
	if ce.Kind.String() != "truncated_wav" {
		t.Fatalf("token = %q", ce.Kind.String())
	}
	if NewError(KindSync, "x").Error() == "" {
		t.Fatal("error string must not be empty")
	}
}

func TestFrameAlignment(t *testing.T) {
	if TicksPerFrame() != 1600 {
		t.Fatalf("ticks per frame = %d, want 1600", TicksPerFrame())
	}
	if got := AlignToFrame(1700); got != 1600 {
		t.Fatalf("AlignToFrame(1700) = %d, want 1600", got)
	}
	if got := AlignToFrame(3200); got != 3200 {
		t.Fatalf("AlignToFrame(3200) = %d, want 3200", got)
	}
}
