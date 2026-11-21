package model

import (
	"errors"
	"strings"
	"testing"

	"mediad/internal/clock"
)

func TestErrorCarriesProvenance(t *testing.T) {
	iv := Interval{Start: 3000, End: 4500, Timebase: clock.Video90k}
	err := Wrap(CategoryMetadata, "parse", errors.New("bad colr")).
		WithTrack("v0").
		WithPacket(77).
		WithInterval(iv).
		WithBlockingDep("decode:v0").
		WithBlockingDep("render:f0")
	msg := err.Error()
	for _, want := range []string{"metadata", "track=v0", "packet=77", "blocked_by=decode:v0", "[3000,4500)", "(33..50 ms)", "bad colr"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error %q missing %q", msg, want)
		}
	}
	var me *Error
	if !errors.As(err, &me) {
		t.Fatal("errors.As failed")
	}
	if CategoryOf(err) != CategoryMetadata {
		t.Fatal("category")
	}
	if FirstBlocking(err) != "decode:v0" {
		t.Fatalf("first blocking dep overwritten: %q", FirstBlocking(err))
	}
}

func TestExitCodes(t *testing.T) {
	if got := ExitCode(nil); got != 0 {
		t.Fatalf("nil = %d", got)
	}
	if got := ExitCode(New(CategoryUsage, "flags", "bad")); got != 1 {
		t.Fatalf("usage = %d", got)
	}
	if got := ExitCode(New(CategoryMetadata, "parse", "bad")); got != 2 {
		t.Fatalf("metadata = %d", got)
	}
	if got := ExitCode(errors.New("plain")); got != 2 {
		t.Fatalf("plain = %d", got)
	}
}

func TestTrackKey(t *testing.T) {
	if TrackKey("video", 0) != "v0" || TrackKey("audio", 2) != "a2" || TrackKey("subtitle", 1) != "t1" {
		t.Fatal("track key normalization")
	}
}

func TestIntervalValidity(t *testing.T) {
	if !(Interval{Start: 0, End: 1, Timebase: clock.Millisecond}).Valid() {
		t.Fatal("valid interval rejected")
	}
	if (Interval{Start: 2, End: 1, Timebase: clock.Millisecond}).Valid() {
		t.Fatal("inverted interval accepted")
	}
}
