package media

import "testing"

func TestRationalRescaleExact(t *testing.T) {
	tb := MustParseRational("1/90000")
	got, err := tb.TicksToTicks(90000, Rational{Num: 1, Den: 48000})
	if err != nil {
		t.Fatal(err)
	}
	if got != 48000 {
		t.Fatalf("got %d", got)
	}
}

func TestRationalOverflowRejected(t *testing.T) {
	if _, err := NewRational(1<<40, 1); err == nil {
		t.Fatal("huge numerator accepted")
	}
	if _, err := NewRational(1, 0); err == nil {
		t.Fatal("zero denominator accepted")
	}
}

func TestTicksToTicksOverflowDetected(t *testing.T) {
	tb := MustParseRational("1/1")
	// Scaling a huge value up into 48 kHz ticks must overflow int64.
	_, err := tb.TicksToTicks(1<<50, Rational{Num: 1, Den: 48000})
	if err == nil {
		t.Fatal("overflow not detected")
	}
}

func TestExitCodes(t *testing.T) {
	if ExitCode(nil) != 0 {
		t.Fatal("nil should be 0")
	}
	if ExitCode(NewError(ErrInvalidTimebase, "x")) != 2 {
		t.Fatal("timebase should be 2")
	}
	if ExitCode(NewError(ErrCorrupt, "x")) != 3 {
		t.Fatal("corrupt should be 3")
	}
}
