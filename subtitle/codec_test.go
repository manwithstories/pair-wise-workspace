package subtitle

import (
	"strings"
	"testing"

	"medcheck/fault"
)

func TestSRTRoundTripPreservesTextAndMilliseconds(t *testing.T) {
	cues := []Cue{
		{StartMS: 0, EndMS: 1234, Text: "开场"},
		{StartMS: 2500, EndMS: 4000, Text: "第二句"},
	}
	got, encoded, err := RoundTrip(cues, FormatSRT)
	if err != nil {
		t.Fatal(err)
	}
	if !Equal(cues, got) {
		t.Fatalf("SRT mismatch:\nencoded=%s\ngot=%+v", encoded, got)
	}
	if !strings.Contains(string(encoded), "00:00:01,234") {
		t.Fatalf("millisecond boundary lost: %s", encoded)
	}
}

func TestVTTRoundTripPreservesTextAndMilliseconds(t *testing.T) {
	cues := []Cue{{StartMS: 1500, EndMS: 2750, Text: "WebVTT ✓"}}
	got, encoded, err := RoundTrip(cues, FormatVTT)
	if err != nil {
		t.Fatal(err)
	}
	if !Equal(cues, got) {
		t.Fatalf("VTT mismatch:\n%s\ngot=%+v", encoded, got)
	}
	if !strings.HasPrefix(string(encoded), "WEBVTT") || !strings.Contains(string(encoded), "00:00:02.750") {
		t.Fatalf("VTT boundary/header wrong: %s", encoded)
	}
}

func TestMalformedAndTruncatedCuesAreIsolatedByLine(t *testing.T) {
	input := `1
00:00:00,000 --> 00:00:01,000
Valid

this block has no timing

3
00:00:04,000 --> 00:00:05,000

4
00:00:06,000 --> bad
Text
`
	doc, err := Parse(strings.NewReader(input), FormatSRT)
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.Cues) != 1 || doc.Cues[0].Text != "Valid" {
		t.Fatalf("valid cue should survive: %+v", doc.Cues)
	}
	kinds := map[fault.Kind]int{}
	lines := map[fault.Kind]int{}
	for _, issue := range doc.Issues {
		kinds[issue.Kind]++
		lines[issue.Kind] = issue.SourceLine
	}
	if kinds[fault.MalformedCue] != 2 || kinds[fault.TruncatedCue] != 1 {
		t.Fatalf("issues=%+v kinds=%v", doc.Issues, kinds)
	}
	if lines[fault.MalformedCue] == 0 || lines[fault.TruncatedCue] == 0 {
		t.Fatalf("source lines not retained: %+v", doc.Issues)
	}
}

func TestOverlapIsReportedBySourceLine(t *testing.T) {
	input := `1
00:00:00,000 --> 00:00:02,000
First

2
00:00:01,500 --> 00:00:03,000
Overlapping
`
	doc, err := Parse(strings.NewReader(input), FormatSRT)
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.Cues) != 2 {
		t.Fatalf("overlapping cues should remain available for editorial marking")
	}
	var found bool
	for _, issue := range doc.Issues {
		if issue.Kind == fault.OverlappingCue && issue.SourceLine == 6 {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected overlap at timing source line 6, got %+v", doc.Issues)
	}
}
