package clock

import "testing"

// referenceDFLabel walks SMPTE drop-frame label space directly to produce the
// ground-truth legal (minute, second, frame) label for every real frame. It is
// a definitional simulation independent of the production closed-form path, so
// the exhaustive test cross-checks two independent implementations.
func referenceDFLabel(frames int64) (min, sec, fr int64) {
	nf := nominalFPS(Rate2997DF)
	d := dropFrames(Rate2997DF)
	minute := int64(0)
	remaining := frames
	for {
		minuteLen := nf * 60
		if minute%10 != 0 {
			minuteLen -= d
		}
		if remaining < minuteLen {
			break
		}
		remaining -= minuteLen
		minute++
	}
	if minute%10 != 0 {
		remaining += d
	}
	sec, fr = remaining/nf, remaining%nf
	return minute, sec, fr
}

// TestDropFrameExhaustive walks every real frame across several ten-minute
// blocks and proves forward (tuple -> frame -> tuple) and inverse conversions
// are exact bijections over the legal label space, matching the independent
// reference walker.
func TestDropFrameExhaustive(t *testing.T) {
	const blocks = 6 // one hour
	fpb := Rate2997DF.FramesPer10Min()
	for frame := int64(0); frame < fpb*blocks; frame++ {
		tc := NanosToTimecode(Rate2997DF, framesToNanos(Rate2997DF, frame))
		if !ValidTimecode(Rate2997DF, tc.Hour, tc.Min, tc.Sec, tc.Frame) {
			t.Fatalf("frame %d produced illegal tuple %s", frame, tc)
		}
		back, err := TimecodeToNanos(Rate2997DF, tc.Hour, tc.Min, tc.Sec, tc.Frame)
		if err != nil {
			t.Fatalf("frame %d tuple %s rejected: %v", frame, tc, err)
		}
		backFrame := Rat{Num: Rate2997DF.Num, Den: Rate2997DF.Den * Second}.CeilDiv(back)
		if backFrame != frame {
			t.Fatalf("frame %d -> %s -> frame %d", frame, tc, backFrame)
		}
		rm, rs, rf := referenceDFLabel(frame)
		if int64(tc.Min)+int64(tc.Hour)*60 != rm || int64(tc.Sec) != rs || int64(tc.Frame) != rf {
			t.Fatalf("frame %d production %s != reference (%d:%d;%d)", frame, tc, rm/60, rm%60, rf)
		}
	}
}

func TestParseTimecode(t *testing.T) {
	tc, err := ParseTimecode(Rate2997DF, "00:01:00;02")
	if err != nil {
		t.Fatal(err)
	}
	if tc.Frame != 2 || tc.Sec != 0 || tc.Min != 1 {
		t.Fatalf("parsed wrong tuple: %s", tc)
	}
	if _, err := ParseTimecode(Rate2997DF, "00:01:00;00"); err == nil {
		t.Fatal("dropped frame must be rejected")
	}
	if _, err := ParseTimecode(Rate25, "garbage"); err == nil {
		t.Fatal("garbage must fail parsing")
	}
	tc25, err := ParseTimecode(Rate25, "01:02:03:04")
	if err != nil || tc25.String() != "01:02:03:04" {
		t.Fatalf("25fps parse wrong: %v %s", err, tc25)
	}
}
