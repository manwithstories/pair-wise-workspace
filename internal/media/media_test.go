package media

import (
	"errors"
	"testing"
)

func TestClockExactRationalConversion(t *testing.T) {
	vid := MustClock(90000)
	aud := MustClock(48000)
	d, err := vid.FromRational(1, 30) // one 29.97-ish frame
	if err != nil {
		t.Fatalf("FromRational: %v", err)
	}
	if d.Ticks() != 3000 {
		t.Fatalf("expected 3000 ticks @90k, got %d", d.Ticks())
	}
	conv, err := d.Convert(aud)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	if conv.Ticks() != 1600 { // 1/30 s == 1600 samples @48k
		t.Fatalf("expected 1600 ticks @48k, got %d", conv.Ticks())
	}
	if got := conv.Milliseconds(); got < 33.3 || got > 33.4 {
		t.Fatalf("ms drift: %v", got)
	}
}

func TestClockRejectsInexact(t *testing.T) {
	c := MustClock(90000)
	if _, err := c.FromRational(1, 7); err == nil {
		t.Fatal("expected inexact rational to fail")
	}
	if _, err := c.FromMilliseconds(1); err != nil { // 90 ticks exactly
		t.Fatalf("1ms should be exact at 90k: %v", err)
	}
	if _, err := MustClock(48000).FromMilliseconds(1); err != nil { // 48 ticks exactly
		t.Fatalf("1ms should be exact at 48k: %v", err)
	}
	if _, err := NewClock(0); !errors.Is(err, ErrInvalidClock) {
		t.Fatalf("want ErrInvalidClock, got %v", err)
	}
}

func TestClockArithmeticAndTicks(t *testing.T) {
	c := MustClock(90000)
	a := c.FromTicks(90000)
	b := c.FromTicks(3000)
	sum, err := a.Add(b)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if sum.Ticks() != 93000 {
		t.Fatalf("sum %d", sum.Ticks())
	}
	if _, err := a.Add(MustClock(48000).FromTicks(1)); err == nil {
		t.Fatal("cross-clock add must fail")
	}
	tick := c.At(1000)
	next, err := tick.Add(c.FromTicks(500))
	if err != nil {
		t.Fatalf("Tick.Add: %v", err)
	}
	if next.Value() != 1500 {
		t.Fatalf("tick %d", next.Value())
	}
	span, err := next.Sub(tick)
	if err != nil || span.Ticks() != 500 {
		t.Fatalf("Sub: %v %d", err, span.Ticks())
	}
	abs := c.FromTicks(-5).Abs()
	if abs.Ticks() != 5 {
		t.Fatalf("Abs %d", abs.Ticks())
	}
}

func TestLCM(t *testing.T) {
	if v, err := LCM(90000, 48000); err != nil || v != 720000 {
		t.Fatalf("LCM 90k/48k = %d, %v", v, err)
	}
	if _, err := LCM(0, 5); err == nil {
		t.Fatal("LCM zero must fail")
	}
	if GCD(90000, 48000) != 6000 {
		t.Fatalf("GCD %d", GCD(90000, 48000))
	}
}

func TestFaultPositionAndRecoverability(t *testing.T) {
	f := NewFault(FaultTruncated, SourcePos{Artifact: "seg1.ts", Offset: 1024, Segment: 2}, "short")
	if !f.Recoverable() {
		t.Fatal("truncation should be quarantinable and recoverable")
	}
	enriched := f.At(SourcePos{Line: 4})
	if enriched.Pos.Line != 4 || enriched.Pos.Offset != 1024 || enriched.Pos.Segment != 2 {
		t.Fatalf("At must only fill unset fields: %+v", enriched.Pos)
	}
	if f.Pos.Line != 0 {
		t.Fatal("At mutated original")
	}
	if got := ExitCode(f); got != 3 {
		t.Fatalf("recoverable exit code %d", got)
	}
	inv := NewFault(FaultStateInvariant, SourcePos{}, "invariant")
	if ExitCode(inv) != 4 {
		t.Fatalf("invariant exit %d", ExitCode(inv))
	}
	if _, ok := AsFault(WrapFault(FaultIO, SourcePos{}, errors.New("disk"))); !ok {
		t.Fatal("wrap should produce a fault")
	}
	if KindOf(errors.New("plain")) != FaultUnknown {
		t.Fatal("plain error should be unknown")
	}
}

func TestBuildTimelineSharedRationalClock(t *testing.T) {
	tl, err := BuildTimeline([]TrackSpec{
		{Kind: KindVideo, Codec: "avc1.64001f", Timescale: 90000, StartTick: 0, DurationTk: 900000, FrameRateNum: 30, FrameRateDen: 1},
		{Kind: KindAudio, Codec: "mp4a.40.2", Timescale: 48000, StartTick: 0, DurationTk: 480000, SampleRate: 48000, Channels: 2},
	})
	if err != nil {
		t.Fatalf("BuildTimeline: %v", err)
	}
	if tl.Ref.TicksPerSecond() != 720000 {
		t.Fatalf("ref clock %d", tl.Ref.TicksPerSecond())
	}
	if got := tl.Span().Milliseconds(); got < 9999 || got > 10001 {
		t.Fatalf("span %v ms", got)
	}
	if tl.AlignMaxMs() != 0 {
		t.Fatalf("aligned tracks offset %v", tl.AlignMaxMs())
	}
}

func TestTimelineNonZeroStartAndDurations(t *testing.T) {
	tl, err := BuildTimeline([]TrackSpec{
		{Kind: KindVideo, Codec: "avc1.64001f", Timescale: 90000, StartTick: 1800, DurationTk: 900000, FrameRateNum: 30, FrameRateDen: 1},
		{Kind: KindAudio, Codec: "mp4a.40.2", Timescale: 48000, StartTick: 960, DurationTk: 480000, SampleRate: 48000, Channels: 2},
	})
	if err != nil {
		t.Fatalf("BuildTimeline: %v", err)
	}
	// video starts at 20ms, audio at 20ms: both offsets equal, align == 0.
	if tl.AlignMaxMs() != 0 {
		t.Fatalf("align %v", tl.AlignMaxMs())
	}
	// both tracks start 20ms in; the timeline keeps the absolute origin at
	// 20 ms and relative offsets are zero.
	if tl.Start.Milliseconds() != 20 {
		t.Fatalf("start should be the absolute 20ms origin, got %v", tl.Start.Milliseconds())
	}
	// audio 20 ms later than video's 0 in a second case.
	tl2, err := BuildTimeline([]TrackSpec{
		{Kind: KindVideo, Codec: "avc1.64001f", Timescale: 90000, DurationTk: 900000, FrameRateNum: 30, FrameRateDen: 1},
		{Kind: KindAudio, Codec: "mp4a.40.2", Timescale: 48000, StartTick: 960, DurationTk: 480000, SampleRate: 48000, Channels: 2},
	})
	if err != nil {
		t.Fatalf("BuildTimeline: %v", err)
	}
	if got := tl2.AlignMaxMs(); got < 19.9 || got > 20.1 {
		t.Fatalf("audio lead should be 20ms, got %v", got)
	}
}

func TestTimelineRejectsBadPackages(t *testing.T) {
	cases := []struct {
		name  string
		specs []TrackSpec
	}{
		{"missing audio", []TrackSpec{{Kind: KindVideo, Codec: "avc1.64001f", Timescale: 90000, DurationTk: 900000, FrameRateNum: 30, FrameRateDen: 1}}},
		{"unsupported pair", []TrackSpec{
			{Kind: KindVideo, Codec: "hvc1.1.6", Timescale: 90000, DurationTk: 900000, FrameRateNum: 30, FrameRateDen: 1},
			{Kind: KindAudio, Codec: "opus", Timescale: 48000, DurationTk: 480000, SampleRate: 48000},
		}},
		{"unknown codec", []TrackSpec{
			{Kind: KindVideo, Codec: "vp9", Timescale: 90000, DurationTk: 900000, FrameRateNum: 30, FrameRateDen: 1},
			{Kind: KindAudio, Codec: "mp4a.40.2", Timescale: 48000, DurationTk: 480000, SampleRate: 48000},
		}},
		{"zero timescale", []TrackSpec{
			{Kind: KindVideo, Codec: "avc1", Timescale: 0, DurationTk: 1, FrameRateNum: 30, FrameRateDen: 1},
			{Kind: KindAudio, Codec: "mp4a", Timescale: 48000, DurationTk: 480000, SampleRate: 48000},
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := BuildTimeline(c.specs)
			if err == nil {
				t.Fatal("expected failure")
			}
			if KindOf(err) != FaultUnknown && KindOf(err) != FaultUnsupportedCodec {
				// errors.Is on the sentinel is the contract
			}
			if !errors.Is(err, ErrMissingTrack) && !errors.Is(err, ErrUnsupportedCodec) && !errors.Is(err, ErrTrackTimescale) {
				t.Fatalf("unexpected sentinel: %v", err)
			}
		})
	}
	if _, _, err := ParseFrameRate("30000/1001"); err != nil {
		t.Fatalf("ParseFrameRate: %v", err)
	}
}
