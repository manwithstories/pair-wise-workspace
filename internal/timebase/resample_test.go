package timebase

import (
	"testing"

	"subtool/internal/subtitle"
)

func TestFrameForMSUsesIntegerRationalMath(t *testing.T) {
	rate, err := ParseRate("24000/1001")
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		ms    int64
		frame int64
	}{
		{1250, 30},
		{3750, 90},
		{1000, 24},
		{0, 0},
	}
	for _, test := range tests {
		if got := FrameForMS(test.ms, rate); got != test.frame {
			t.Fatalf("FrameForMS(%d)=%d want %d", test.ms, got, test.frame)
		}
	}
}

func TestParseRateReducesRationals(t *testing.T) {
	rate, err := ParseRate("30000/1001")
	if err != nil {
		t.Fatal(err)
	}
	if rate.Num != 30000 || rate.Den != 1001 || rate.String() != "30000/1001" {
		t.Fatalf("unexpected rate: %#v", rate)
	}
	integer, err := ParseRate("25")
	if err != nil || integer.Num != 25 || integer.Den != 1 {
		t.Fatalf("unexpected integer rate: %#v %v", integer, err)
	}
}

func TestResampleModes(t *testing.T) {
	source := &subtitle.Track{Cues: []subtitle.Cue{
		{Index: 1, StartMS: 1000, EndMS: 2000, Text: "a"},
	}}
	subtitle.Normalize(source)
	inRate, _ := ParseRate("25")
	outRate, _ := ParseRate("30")
	aligned, err := Resample(source, inRate, outRate, false)
	if err != nil {
		t.Fatal(err)
	}
	if aligned.Cues[0].StartMS != 1000 || aligned.Cues[0].EndMS != 2000 {
		t.Fatalf("align-only must preserve time: %#v", aligned.Cues[0])
	}
	scaled, err := Resample(source, inRate, outRate, true)
	if err != nil {
		t.Fatal(err)
	}
	if scaled.Cues[0].StartMS != 1200 || scaled.Cues[0].EndMS != 2400 {
		t.Fatalf("speed-follow scale mismatch: %#v", scaled.Cues[0])
	}
}

func TestAlignMS(t *testing.T) {
	rate, _ := ParseRate("25")
	if got := AlignMS(1250, rate); got != 1240 {
		t.Fatalf("AlignMS(1250,25)=%d want 1240", got)
	}
}
