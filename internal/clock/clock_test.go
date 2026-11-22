package clock

import (
	"errors"
	"testing"
)

func TestRateConversions(t *testing.T) {
	opus := Rate(48000)
	if got := opus.USToTicks(20000); got != 960 {
		t.Fatalf("20ms at 48k = %d ticks, want 960", got)
	}
	if got := opus.TicksToUS(960); got != 20000 {
		t.Fatalf("960 ticks at 48k = %d us, want 20000", got)
	}
	aac := Rate(90000)
	if got := aac.USToTicks(21333); got != 1919 { // truncated, deterministic
		t.Fatalf("21333us at 90k = %d ticks, want 1919", got)
	}
	if err := Rate(0).Validate(); !errors.Is(err, ErrZeroRate) {
		t.Fatalf("zero rate error = %v", err)
	}
}

func TestTimestampUnwrapperWrap(t *testing.T) {
	var u TimestampUnwrapper
	base := uint32(0xFFFFFF00)
	if got := u.Unwrap(base); got != int64(base) {
		t.Fatalf("first = %d", got)
	}
	// 512 ticks later, crossing the 32-bit wrap.
	got := u.Unwrap(base + 512)
	if want := int64(base) + 512; got != want {
		t.Fatalf("wrapped = %d, want %d", got, want)
	}
	// Reordered older packet still maps to a lower extended value.
	if got := u.Unwrap(base + 100); got != int64(base)+100 {
		t.Fatalf("reordered = %d", got)
	}
}

func TestSequenceUnwrapperWrap(t *testing.T) {
	var u SequenceUnwrapper
	if got := u.Unwrap(65534); got != 65534 {
		t.Fatalf("first = %d", got)
	}
	if got := u.Unwrap(1); got != 65537 {
		t.Fatalf("wrap = %d, want 65537", got)
	}
	if got := u.Unwrap(65535); got != 65535 {
		t.Fatalf("reordered = %d, want 65535", got)
	}
}

func TestUnwrapperStateRoundTrip(t *testing.T) {
	var u TimestampUnwrapper
	u.Unwrap(100)
	last, ext, ok := u.State()
	var restored TimestampUnwrapper
	restored.Restore(last, ext, ok)
	if got := restored.Unwrap(50); got != 50 {
		t.Fatalf("restored unwrap = %d", got)
	}
}

func TestValidateJumpAndDuration(t *testing.T) {
	if err := ValidateJump(1000); err != nil {
		t.Fatalf("small jump rejected: %v", err)
	}
	if err := ValidateJump(MaxForwardJumpUS + 1); err == nil {
		t.Fatal("forward clock jump accepted")
	} else {
		var je *JumpError
		if !errors.As(err, &je) {
			t.Fatalf("error type = %T", err)
		}
	}
	if err := ValidateJump(-MaxBackwardJumpUS - 1); err == nil {
		t.Fatal("backward clock jump accepted")
	}
	if err := ValidateDuration(0); !errors.Is(err, ErrNonPositiveDuration) {
		t.Fatalf("zero duration err = %v", err)
	}
	if err := ValidateDuration(-1); !errors.Is(err, ErrNonPositiveDuration) {
		t.Fatalf("negative duration err = %v", err)
	}
}
