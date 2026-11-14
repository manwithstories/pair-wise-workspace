package verify

import (
	"testing"

	"subtool/internal/subtitle"
	"subtool/internal/timebase"
)

func TestCompareAndPassRules(t *testing.T) {
	expected := &subtitle.Track{Cues: []subtitle.Cue{
		{Index: 1, StartMS: 0, EndMS: 1000, Text: "a"},
		{Index: 2, StartMS: 2000, EndMS: 3000, Text: `{\p1}draw`},
	}}
	subtitle.Normalize(expected)
	subtitle.PrepareForFormat(expected, subtitle.FormatSRT)
	actual := &subtitle.Track{Cues: []subtitle.Cue{
		{Index: 1, StartMS: 3, EndMS: 997, Text: "a"},
	}}
	inRate, _ := timebase.ParseRate("25")
	outRate, _ := timebase.ParseRate("30000/1001")
	result := Compare(expected, actual, inRate, outRate)
	if result.DroppedTotal != 1 || result.Dropped[0].Reason != "unsupported_ass_drawing" {
		t.Fatalf("dropped reason missing: %#v", result.Dropped)
	}
	if !result.CueCountMatch || result.MaxAbsDriftMS != 3 {
		t.Fatalf("unexpected comparison: %#v", result)
	}
	if !result.Passed(0, subtitle.FormatASS) {
		t.Fatal("ASS quantization within 5ms should pass")
	}
	if result.Passed(0, subtitle.FormatVTT) {
		t.Fatal("SRT/VTT drift must exceed tolerance")
	}
	if result.QuantizedMaxMS(subtitle.FormatASS) != 3 {
		t.Fatalf("quantized max=%d", result.QuantizedMaxMS(subtitle.FormatASS))
	}
}

func TestCompareTextMismatch(t *testing.T) {
	expected := &subtitle.Track{Cues: []subtitle.Cue{{Index: 1, StartMS: 0, EndMS: 1000, Text: "a"}}}
	actual := &subtitle.Track{Cues: []subtitle.Cue{{Index: 1, StartMS: 0, EndMS: 1000, Text: "b"}}}
	rate, _ := timebase.ParseRate("25")
	result := Compare(expected, actual, rate, rate)
	if result.TextMismatches != 1 || result.Passed(0, subtitle.FormatVTT) {
		t.Fatalf("text mismatch was accepted: %#v", result)
	}
}
