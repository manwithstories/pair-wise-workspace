package media

import (
	"errors"
	"testing"
	"time"
)

func TestClockRoundTripExact(t *testing.T) {
	clock, err := NewClock(Rate{Num: 48000, Den: 1})
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []time.Duration{
		0,
		time.Millisecond,
		20 * time.Millisecond,
		time.Second,
		123456789 * time.Nanosecond,
	} {
		frames := clock.Frames(d)
		back := clock.Duration(frames)
		// One frame at 48 kHz lasts 20.8 us; the round trip can only be
		// accurate to half a frame.
		if diff := back - d; diff < -11*time.Microsecond || diff > 11*time.Microsecond {
			t.Fatalf("%v -> %d frames -> %v", d, frames, back)
		}
	}
}

func TestClockConvertBlocksDoNotDrift(t *testing.T) {
	src, _ := NewClock(Rate{Num: 44100, Den: 1})
	dstRate := Rate{Num: 48000, Den: 1}
	var lastDst int64
	for block := int64(0); block < 5000; block++ {
		start := block * 441
		end := start + 441
		s, err := src.Convert(start, dstRate)
		if err != nil {
			t.Fatal(err)
		}
		e, err := src.Convert(end, dstRate)
		if err != nil {
			t.Fatal(err)
		}
		if s != lastDst {
			t.Fatalf("block %d start %d, previous end %d", block, s, lastDst)
		}
		if e-s != 480 && e-s != 479 && e-s != 481 {
			t.Fatalf("block %d length %d", block, e-s)
		}
		lastDst = e
	}
	total, _ := src.Convert(5000*441, dstRate)
	if lastDst != total {
		t.Fatalf("accumulated end %d != exact total %d", lastDst, total)
	}
}

func TestClock48kTo16kIsExact(t *testing.T) {
	src, _ := NewClock(Rate{Num: 48000, Den: 1})
	dst := Rate{Num: 16000, Den: 1}
	for f := int64(0); f <= 48000*10; f += 160 {
		got, err := src.Convert(f, dst)
		if err != nil {
			t.Fatal(err)
		}
		if got != (f+1)/3 {
			t.Fatalf("frame %d -> %d want %d", f, got, (f+1)/3)
		}
	}
}

func TestRateValidation(t *testing.T) {
	if _, err := NewRate(0, 1); err == nil {
		t.Fatal("zero rate accepted")
	}
	if _, err := NewRate(1, 0); err == nil {
		t.Fatal("zero denominator accepted")
	}
	if _, err := NewClock(Rate{}); err == nil {
		t.Fatal("zero clock accepted")
	}
}

func TestClockMillis(t *testing.T) {
	clock, _ := NewClock(Rate{Num: 48000, Den: 1})
	if got := clock.Millis(480); got != 10 {
		t.Fatalf("Millis(480)=%d want 10", got)
	}
	if got := clock.Millis(43200); got != 900 {
		t.Fatalf("Millis(43200)=%d want 900", got)
	}
}

func TestBlockValidate(t *testing.T) {
	b := Block{Track: "a", Channels: 2, Rate: Rate{Num: 48000, Den: 1}, Samples: make([]float64, 200)}
	if err := b.Validate(50); err == nil {
		t.Fatal("expected frame limit error")
	}
	if err := b.Validate(100); err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	b.Samples = b.Samples[:199]
	if err := b.Validate(100); err == nil {
		t.Fatal("expected alignment error")
	}
	if b.Frames() != 99 {
		t.Fatalf("Frames()=%d", b.Frames())
	}
}

func TestErrorContextAndExit(t *testing.T) {
	cause := errors.New("boom")
	err := NewError(PhaseResample, ExitInput, "audio", 7, true, "fir", cause)
	if err == nil {
		t.Fatal("nil error")
	}
	if !errors.Is(err, cause) {
		t.Fatal("cause not wrapped")
	}
	pe, ok := AsError(err)
	if !ok || pe.Track != "audio" || pe.Block != 7 || !pe.Recoverable {
		t.Fatalf("bad context: %+v", pe)
	}
	if ExitFor(err) != ExitInput {
		t.Fatalf("exit %d", ExitFor(err))
	}
	if ExitFor(cause) != ExitInternal {
		t.Fatal("plain error should be internal")
	}
	if ExitFor(nil) != ExitOK {
		t.Fatal("nil should be ok")
	}
	if NewError(PhaseInput, ExitInput, "", 0, false, "x", nil) != nil {
		t.Fatal("nil cause should yield nil")
	}
}
