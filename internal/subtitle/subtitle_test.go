package subtitle

import (
	"testing"

	"pairflow/media/internal/clock"
)

func TestParseSRTBasicAndOffsets(t *testing.T) {
	src := "1\r\n00:00:01,000 --> 00:00:02,500\r\nhello\r\n\r\n" +
		"2\r\n00:00:03,000 --> 00:00:04,000\r\nworld\r\n"
	doc := ParseSRT([]byte(src), "sub-en")
	if len(doc.Cues) != 2 || len(doc.Errors) != 0 {
		t.Fatalf("cues=%d errors=%+v", len(doc.Cues), doc.Errors)
	}
	if !doc.CRLF {
		t.Fatal("expected CRLF detection")
	}
	c := doc.Cues[0]
	if c.Start != clock.NewRational(1, 1) || c.End != clock.NewRational(5, 2) {
		t.Fatalf("times = %s..%s", c.Start, c.End)
	}
	if c.Offset != 0 {
		t.Fatalf("first cue offset = %d, want 0 (record start)", c.Offset)
	}
	if c.Text != "hello" {
		t.Fatalf("text = %q", c.Text)
	}
	// Second cue must point at its own sequence line.
	wantOff := int64(len("1\r\n00:00:01,000 --> 00:00:02,500\r\nhello\r\n\r\n"))
	if doc.Cues[1].Offset != wantOff {
		t.Fatalf("second cue offset = %d, want %d", doc.Cues[1].Offset, wantOff)
	}
}

func TestParseSRTBOMAndLF(t *testing.T) {
	src := "\xEF\xBB\xBF1\n00:00:00,500 --> 00:00:01,000\nbom\n"
	doc := ParseSRT([]byte(src), "sub")
	if !doc.BOM || doc.CRLF {
		t.Fatalf("flags bom=%v crlf=%v", doc.BOM, doc.CRLF)
	}
	if len(doc.Cues) != 1 || doc.Cues[0].Offset != 3 {
		t.Fatalf("cues=%+v", doc.Cues)
	}
	if doc.Cues[0].Start != clock.NewRational(1, 2) {
		t.Fatalf("start = %s", doc.Cues[0].Start)
	}
}

func TestParseSRTRecoversFromBadRecord(t *testing.T) {
	src := "1\n00:00:01,000 --> 00:00:02,000\nok\n\n" +
		"2\n00:00:0x,000 --> 00:00:03,000\nbroken\n\n" +
		"3\n00:00:05,000 --> 00:00:06,000\nrecovered\n"
	doc := ParseSRT([]byte(src), "sub")
	if len(doc.Cues) != 2 {
		t.Fatalf("cues = %d, want 2", len(doc.Cues))
	}
	if len(doc.Errors) != 1 {
		t.Fatalf("errors = %+v", doc.Errors)
	}
	e := doc.Errors[0]
	if e.Code != "bad_timestamp" || e.Offset <= 0 {
		t.Fatalf("error = %+v", e)
	}
	if doc.Cues[1].Text != "recovered" {
		t.Fatalf("recovered cue text = %q", doc.Cues[1].Text)
	}
}

func TestParseSRTTruncatedTail(t *testing.T) {
	src := "1\n00:00:01,000 --> 00:00:02,000\ncomplete\n\n2\n00:00:03,000 --> 00:00:04,000\ntrunc"
	doc := ParseSRT([]byte(src), "sub")
	if len(doc.Cues) != 2 {
		t.Fatalf("cues = %d", len(doc.Cues))
	}
	if !doc.Truncated {
		t.Fatal("expected truncated flag")
	}
	if doc.Cues[1].Text != "trunc" {
		t.Fatalf("tail text = %q", doc.Cues[1].Text)
	}
}

func TestParseSRTNegativeDurationRejected(t *testing.T) {
	src := "1\n00:00:05,000 --> 00:00:04,000\nbackwards\n"
	doc := ParseSRT([]byte(src), "sub")
	if len(doc.Cues) != 0 || len(doc.Errors) != 1 {
		t.Fatalf("cues=%d errors=%+v", len(doc.Cues), doc.Errors)
	}
	if doc.Errors[0].Code != "negative_duration" {
		t.Fatalf("code = %s", doc.Errors[0].Code)
	}
}

func TestParseVTTBasic(t *testing.T) {
	src := "WEBVTT\n\nNOTE this is a comment\n\n" +
		"intro\n00:00:01.000 --> 00:00:02.250 align:start position:10%\n<v Bob>Hi</v>\n\n" +
		"00:00:03.000 --> 00:00:04.000\nsecond\n"
	doc := ParseVTT([]byte(src), "sub-vtt")
	if len(doc.Cues) != 2 || len(doc.Errors) != 0 {
		t.Fatalf("cues=%d errors=%+v", len(doc.Cues), doc.Errors)
	}
	c := doc.Cues[0]
	if c.Sequence != "intro" {
		t.Fatalf("identifier = %q", c.Sequence)
	}
	if c.Settings != "align:start position:10%" {
		t.Fatalf("settings = %q", c.Settings)
	}
	if c.End != clock.NewRational(9, 4) {
		t.Fatalf("end = %s", c.End)
	}
}

func TestParseVTTMissingSignatureAndBadCue(t *testing.T) {
	src := "00:00:01.000 --> 00:00:02.000\nfirst\n\n" +
		"00:00:03.000 --> broken\nsecond\n"
	doc := ParseVTT([]byte(src), "sub-vtt")
	if len(doc.Errors) != 2 {
		t.Fatalf("errors = %+v", doc.Errors)
	}
	if doc.Errors[0].Code != "missing_signature" || doc.Errors[0].Offset != 0 {
		t.Fatalf("signature error = %+v", doc.Errors[0])
	}
	if doc.Errors[1].Code != "bad_timestamp" || doc.Errors[1].Offset <= 0 {
		t.Fatalf("bad cue error = %+v", doc.Errors[1])
	}
	if len(doc.Cues) != 1 {
		t.Fatalf("cues = %d", len(doc.Cues))
	}
}

func TestTimestampParsing(t *testing.T) {
	good := map[string]string{
		"00:00:01,500": "3/2",
		"0:00:02.25":   "9/4",
		"00:01:00,000": "60/1",
		"01:00:00,001": "3600001/1000",
		"12:34,500":    "1509/2",
		"7":            "7/1",
	}
	for in, want := range good {
		got, ok := timestamp(in)
		if !ok || got.String() != want {
			t.Errorf("timestamp(%q) = %s, %v; want %s", in, got, ok, want)
		}
	}
	for _, in := range []string{"", "aa:bb", "00:70:00,000", "00:00:60,000", "00:00:01,x1"} {
		if _, ok := timestamp(in); ok {
			t.Errorf("timestamp(%q) unexpectedly ok", in)
		}
	}
}
