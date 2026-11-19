package clock

import "testing"

func TestFrameTickRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		r    Rate
	}{
		{"24", Rate24}, {"25", Rate25}, {"30", Rate30},
		{"2997ndf", Rate2997}, {"2997df", Rate2997DF},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, frame := range []int64{0, 1, 23, 999, 17982, 100000, 107891} {
				tt, err := FrameToTick(tc.r, frame)
				if err != nil {
					t.Fatalf("FrameToTick(%d): %v", frame, err)
				}
				back, err := TickToFrame(tc.r, tt)
				if err != nil || back != frame {
					t.Fatalf("roundtrip frame %d -> %d ticks -> %d, err=%v", frame, tt, back, err)
				}
			}
		})
	}
}

func TestFrameDurationsAreExact(t *testing.T) {
	want := map[Rate]int64{
		Rate24: 1250, Rate25: 1200, Rate30: 1000, Rate2997: 1001, Rate2997DF: 1001,
	}
	for r, d := range want {
		got, err := r.FrameDurTicks()
		if err != nil || got != d {
			t.Fatalf("%v dur=%d want %d err=%v", r, got, d, err)
		}
	}
}

func TestSMPTEDropFrame(t *testing.T) {
	r := Rate2997DF
	// 00:01:00;02 is the first valid label after the skipped pair.
	tt, err := ParseSMPTETick(r, "00:01:00;02", false)
	if err != nil {
		t.Fatal(err)
	}
	back, err := TickToSMPTE(r, tt, false)
	if err != nil || back != "00:01:00;02" {
		t.Fatalf("label roundtrip=%q err=%v", back, err)
	}
	// 00:01:00;00 is a dropped number and must be rejected.
	if _, err := ParseSMPTETick(r, "00:01:00;00", false); err == nil {
		t.Fatal("dropped frame number accepted")
	}
	// Every tenth minute keeps frame 0.
	if _, err := ParseSMPTETick(r, "00:10:00;00", false); err != nil {
		t.Fatal(err)
	}
	if FramesPerDay(r) != 2589408 {
		t.Fatalf("DF frames/day=%d want 2589408", FramesPerDay(r))
	}
}

func TestSMPTENDF(t *testing.T) {
	if FramesPerDay(Rate25) != 2160000 {
		t.Fatal("bad 25fps day count")
	}
	tt, err := ParseSMPTETick(Rate30, "01:00:00:00", false)
	if err != nil || tt != Tick(TickRate*3600) {
		t.Fatalf("30fps hour tick=%d err=%v", tt, err)
	}
	if _, err := ParseSMPTETick(Rate25, "00:00:00:25", false); err == nil {
		t.Fatal("illegal frame 25 accepted at 25fps")
	}
}

func TestDayWrap(t *testing.T) {
	if got := WrapDay(Tick(TickPerDay + 1000)); got != 1000 {
		t.Fatalf("WrapDay=%d", got)
	}
	r := Rate30
	dayEnd := Tick(TickPerDay)
	if _, err := TickToSMPTE(r, dayEnd, false); err == nil {
		t.Fatal("cross-day instant accepted without wrap")
	}
	label, err := TickToSMPTE(r, dayEnd+MillisToTick(500), true)
	if err != nil || label != "00:00:00:15" {
		t.Fatalf("wrap label=%q err=%v", label, err)
	}
}

func TestRTPAndMP4(t *testing.T) {
	rt, err := RTPToTick(90000, 90000) // one RTP second
	if err != nil || rt != Tick(TickRate) {
		t.Fatalf("rtp tick=%d err=%v", rt, err)
	}
	back, err := TickToRTP(rt, 90000)
	if err != nil || back != 90000 {
		t.Fatalf("rtp back=%d err=%v", back, err)
	}
	mt, err := MP4ToTick(1000, 1000) // one MP4 second
	if err != nil || mt != Tick(TickRate) {
		t.Fatalf("mp4 tick=%d err=%v", mt, err)
	}
	if _, err := RTPToTick(1, 90001); err == nil {
		t.Fatal("unaligned RTP timestamp accepted")
	}
}

func TestParseRate(t *testing.T) {
	r, err := ParseRate("30000/1001")
	if err != nil || r.Num != 30000 || r.Den != 1001 {
		t.Fatalf("parse=%+v err=%v", r, err)
	}
	if _, err := ParseRate("0"); err == nil {
		t.Fatal("zero rate accepted")
	}
	if _, err := ParseRate("a/b"); err == nil {
		t.Fatal("garbage rate accepted")
	}
}

func TestMillis(t *testing.T) {
	if MillisToTick(1) != 30 {
		t.Fatal("millis mapping wrong")
	}
	if Tick(45).RoundMillis() != 2 || Tick(15).RoundMillis() != 1 {
		t.Fatal("rounding wrong")
	}
}
