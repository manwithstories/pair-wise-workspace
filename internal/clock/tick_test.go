package clock

import (
	"testing"

	"aircheck/internal/fault"
)

func TestFromRationalExact(t *testing.T) {
	got, err := FromRational(1024, 48000, "a0")
	if err != nil {
		t.Fatal(err)
	}
	if want := int64(1920); got != want {
		t.Fatalf("got %d want %d", got, want)
	}
}

func TestFromRationalRounding(t *testing.T) {
	// 1/44100 s = 2.0408... ticks -> 2
	got, err := FromRational(1, 44100, "a0")
	if err != nil {
		t.Fatal(err)
	}
	if got != 2 {
		t.Fatalf("got %d want 2", got)
	}
}

func TestFromRationalOverflow(t *testing.T) {
	_, err := FromRational(1<<62, 1, "v0")
	if fe := fault.Of(err); fe == nil || fe.Code != fault.CodeOverflow {
		t.Fatalf("want overflow fault, got %v", err)
	}
	if fe := fault.Of(err); fe.Track != "v0" {
		t.Fatalf("track not propagated: %+v", fe)
	}
}

func TestRoundTrip(t *testing.T) {
	tick, err := FromRational(123456789, 90000, "v0")
	if err != nil {
		t.Fatal(err)
	}
	if got := ToRational(tick, 90000); got != 123456789 {
		t.Fatalf("round trip failed: %d", got)
	}
}

func TestUnwrap33(t *testing.T) {
	prev := Wrap33 - 100
	raw := int64(50) // wrapped forward
	got := Unwrap33(prev, raw)
	if got != Wrap33+50 {
		t.Fatalf("got %d want %d", got, Wrap33+50)
	}
}

func TestCheckMonotonic(t *testing.T) {
	if err := CheckMonotonic(100, 200, "v0"); err != nil {
		t.Fatal(err)
	}
	err := CheckMonotonic(200, 100, "v0")
	fe := fault.Of(err)
	if fe == nil || fe.Code != fault.CodeBackwardPTS || fe.Tick != 100 || fe.Track != "v0" {
		t.Fatalf("bad fault: %+v", fe)
	}
}

func TestCheckDuration(t *testing.T) {
	if err := CheckDuration(-5, "a1"); fault.Of(err) == nil || fault.Of(err).Code != fault.CodeNegativeDur {
		t.Fatalf("want negative duration fault, got %v", err)
	}
	if err := CheckDuration(5, "a1"); err != nil {
		t.Fatal(err)
	}
}

func TestMillisRoundTrip(t *testing.T) {
	if got := FromMillis(Millis(180000)); got != 180000 {
		t.Fatalf("got %d", got)
	}
}
