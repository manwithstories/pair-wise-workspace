package subtitle

import (
	"strings"
	"testing"
)

const goodSRT = "1\n00:00:00,000 --> 00:00:01,500\nhello world\n\n" +
	"2\n00:00:02,000 --> 00:00:03,500\nsecond cue\n\n"

func TestParseSRTGood(t *testing.T) {
	cues, err := ParseSRT([]byte(goodSRT))
	if err != nil {
		t.Fatal(err)
	}
	if len(cues) != 2 {
		t.Fatalf("got %d cues", len(cues))
	}
	if cues[0].Start != 0 || cues[0].End != 1_500_000_000 {
		t.Fatalf("cue0 = [%d,%d)", cues[0].Start, cues[0].End)
	}
	if cues[1].Offset != int64(strings.Index(goodSRT, "2\n")) {
		t.Fatalf("cue1 offset = %d", cues[1].Offset)
	}
	if cues[1].Text != "second cue" {
		t.Fatalf("cue1 text = %q", cues[1].Text)
	}
}

func TestSRTDurationErrorOffset(t *testing.T) {
	// end <= start must be reported at the timing line offset.
	src := "1\n00:00:05,000 --> 00:00:01,000\nbad\n"
	cues, err := ParseSRT([]byte(src))
	if len(cues) != 0 {
		t.Fatalf("expected no cues, got %d", len(cues))
	}
	pe, ok := err.(*ParseError)
	if !ok {
		t.Fatalf("err type %T", err)
	}
	if pe.Kind != "duration" {
		t.Fatalf("kind = %s", pe.Kind)
	}
	if pe.Offset != int64(strings.Index(src, "00:00:05")) {
		t.Fatalf("offset = %d, want %d", pe.Offset, strings.Index(src, "00:00:05"))
	}
}

func TestSRTOrderError(t *testing.T) {
	src := "1\n00:00:05,000 --> 00:00:06,000\na\n\n2\n00:00:01,000 --> 00:00:02,000\nb\n"
	_, err := ParseSRT([]byte(src))
	pe, ok := err.(*ParseError)
	if !ok || pe.Kind != "order" {
		t.Fatalf("err = %v", err)
	}
	if pe.Offset != int64(strings.Index(src, "2\n00:00:01")) {
		t.Fatalf("offset = %d", pe.Offset)
	}
}

func TestSRTBadTimestamp(t *testing.T) {
	src := "1\n00:00:0x,000 --> 00:00:01,000\nbad\n"
	if _, err := ParseSRT([]byte(src)); err == nil {
		t.Fatal("expected error")
	}
}

const goodVTT = "WEBVTT\n\n00:00:01.000 --> 00:00:02.500\nhello\n\n" +
	"id-2\n00:03.000 --> 00:04.000\nworld\n"

func TestParseVTTGood(t *testing.T) {
	cues, err := ParseVTT([]byte(goodVTT))
	if err != nil {
		t.Fatal(err)
	}
	if len(cues) != 2 {
		t.Fatalf("got %d cues", len(cues))
	}
	if cues[0].Start != 1_000_000_000 || cues[0].End != 2_500_000_000 {
		t.Fatalf("cue0 = [%d,%d)", cues[0].Start, cues[0].End)
	}
	// Short MM:SS.mmm form.
	if cues[1].Start != 3_000_000_000 {
		t.Fatalf("cue1 start = %d", cues[1].Start)
	}
}

func TestVTTErrors(t *testing.T) {
	if _, err := ParseVTT([]byte("not a vtt")); err == nil {
		t.Fatal("expected header error")
	}
	src := "WEBVTT\n\n00:00:02.000 --> 00:00:01.000\nneg\n"
	_, err := ParseVTT([]byte(src))
	pe, ok := err.(*ParseError)
	if !ok || pe.Kind != "duration" {
		t.Fatalf("err = %v", err)
	}
	if pe.Offset != int64(strings.Index(src, "00:00:02")) {
		t.Fatalf("offset = %d", pe.Offset)
	}
}
