package parse

import (
	"testing"
)

func TestParseTimecodeVariants(t *testing.T) {
	cases := map[string]int64{
		"01:02:03,456": 3723456,
		"01:02:03.456": 3723456,
		"1:02:03.45":   3723450, // ASS centiseconds
		"0:00:01,001":  1001,
	}
	for in, want := range cases {
		r, err := ParseTimecode(in)
		if err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		if !r.IsInt() || r.Num().Int64() != want {
			t.Errorf("%s: got %s want %d", in, r, want)
		}
	}
	if _, err := ParseTimecode("bogus"); err == nil {
		t.Error("expected error for malformed timecode")
	}
}

func TestSRTQuarantine(t *testing.T) {
	src := "1\n25:00:00,000 --> 25:00:01,000\nover 24h\n\n" +
		"2\n00:00:05,000 --> 00:00:05,000\nzero\n\n" +
		"3\n00:00:10,000 --> 00:00:09,000\nreversed\n\n" +
		"4\n00:00:12,000 --> 00:00:13,500\nok\n\n"
	res := ParseSRT(src)
	if len(res.Cues) != 1 {
		t.Fatalf("expected 1 good cue, got %d", len(res.Cues))
	}
	if len(res.Issues) != 3 {
		t.Fatalf("expected 3 issues, got %d: %v", len(res.Issues), res.Issues)
	}
	codes := map[string]bool{}
	for _, is := range res.Issues {
		codes[is.Code] = true
		if is.Line <= 0 {
			t.Errorf("issue missing line number: %+v", is)
		}
	}
	for _, want := range []string{"OVER_24H", "ZERO_LENGTH", "END_BEFORE_START"} {
		if !codes[want] {
			t.Errorf("missing quarantine code %s", want)
		}
	}
}

func TestVTTHeaderAndIDs(t *testing.T) {
	src := "WEBVTT - demo\n\nSTYLE\n::cue { color: lime }\n\n" +
		"cue-7\n00:00:01.000 --> 00:00:02.000\nhello <i>world</i>\n\n" +
		"00:00:03.000 --> 00:00:04.000\nno id\n\n"
	res := ParseVTT(src)
	if len(res.Cues) != 2 {
		t.Fatalf("expected 2 cues, got %d", len(res.Cues))
	}
	if res.Cues[0].ID != "cue-7" {
		t.Errorf("cue id lost: %q", res.Cues[0].ID)
	}
	if res.Header["STYLE"] == "" {
		t.Error("STYLE block not preserved")
	}
	if len(res.Cues[0].Tags) != 2 {
		t.Errorf("inline tags not extracted: %v", res.Cues[0].Tags)
	}
}

func TestTTMLTimeExpressions(t *testing.T) {
	// ticks
	r, err := ParseTTMLTime("100t", 50, 25)
	if err != nil || r.Num().Int64() != 2000 {
		t.Errorf("100t@50: got %v err %v", r, err)
	}
	// frames
	r, _ = ParseTTMLTime("50f", 1, 25)
	if r.Num().Int64() != 2000 {
		t.Errorf("50f@25: got %v", r)
	}
	// clock with fraction
	r, _ = ParseTTMLTime("00:00:01.5", 1, 25)
	if r.Num().Int64() != 1500 {
		t.Errorf("1.5s clock: got %v", r)
	}
	// offset
	r, _ = ParseTTMLTime("2.5s", 1, 25)
	if r.Num().Int64() != 2500 {
		t.Errorf("2.5s: got %v", r)
	}
}
