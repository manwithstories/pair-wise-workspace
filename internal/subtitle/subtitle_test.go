package subtitle

import (
	"strings"
	"testing"
)

func TestParseSRTBasicAndOffsets(t *testing.T) {
	content := "1\n00:00:01,000 --> 00:00:02,500\nhello\n\n2\n00:00:03,000 --> 00:00:04,000\nworld\n"
	res, err := ParseSRT(strings.NewReader(content), "sample.srt")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Events) != 2 {
		t.Fatalf("events = %d, want 2", len(res.Events))
	}
	ev := res.Events[0]
	if ev.StartMS != 1000 || ev.EndMS != 2500 || ev.Text != "hello" {
		t.Fatalf("first event wrong: %+v", ev)
	}
	// Raw offsets must point at the actual bytes of the record.
	if content[ev.RawStart:ev.RawStart+1] != "1" {
		t.Fatalf("RawStart %d does not point at record index", ev.RawStart)
	}
	if content[ev.RawStart:ev.RawEnd] != "1\n00:00:01,000 --> 00:00:02,500\nhello\n" {
		t.Fatalf("raw span wrong: %q", content[ev.RawStart:ev.RawEnd])
	}
	if len(res.Faults) != 0 {
		t.Fatalf("unexpected faults: %v", res.Faults)
	}
}

func TestParseSRTFaultTolerance(t *testing.T) {
	// Record 2 has an invalid time; record 3 overlaps record 1's end.
	content := "1\n00:00:01,000 --> 00:00:03,000\ngood\n\n" +
		"2\n00:00:99,000 --> 00:00:05,000\nbad\n\n" +
		"3\n00:00:02,000 --> 00:00:06,000\noverlap\n"
	res, err := ParseSRT(strings.NewReader(content), "bad.srt")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Events) != 2 {
		t.Fatalf("recoverable events = %d, want 2", len(res.Events))
	}
	var sawInvalid, sawOverlap bool
	for _, f := range res.Faults {
		if !f.Recoverable || f.Stage != "subtitle" {
			t.Fatalf("fault must be recoverable subtitle fault: %+v", f)
		}
		if strings.Contains(f.Message, "invalid SRT timing") {
			sawInvalid = true
		}
		if strings.Contains(f.Message, "overlap") {
			sawOverlap = true
		}
	}
	if !sawInvalid || !sawOverlap {
		t.Fatalf("expected invalid-timing and overlap faults, got %+v", res.Faults)
	}
	// The invalid record's fault must carry its real byte offset.
	if res.Faults[0].Offset < 0 || content[res.Faults[0].Offset] != '2' {
		t.Fatalf("fault offset %d does not anchor at record 2", res.Faults[0].Offset)
	}
}

func TestParseSRTHalfLineTruncation(t *testing.T) {
	// First cue complete; second cue's payload is cut mid-line, no newline.
	content := "1\n00:00:01,000 --> 00:00:02,000\nok\n\n2\n00:00:03,000 --> 00:00:04,000\nhalf"
	res, err := ParseSRT(strings.NewReader(content), "trunc.srt")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Events) != 2 {
		t.Fatalf("events = %d, want 2 (truncated cue retained)", len(res.Events))
	}
	if res.Events[1].Text != "half" {
		t.Fatalf("truncated payload lost: %q", res.Events[1].Text)
	}
	found := false
	for _, f := range res.Faults {
		if strings.Contains(f.Message, "truncated") {
			found = true
		}
	}
	if !found {
		t.Fatal("truncated final record must be flagged")
	}
}

func TestParseVTTBasic(t *testing.T) {
	content := "WEBVTT\n\n" +
		"cue-1\n00:01.000 --> 00:02.500 align:start\nhello\n\n" +
		"01:00:03.000 --> 01:00:04.000\nworld\n"
	res, err := ParseVTT(strings.NewReader(content), "sample.vtt")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Events) != 2 {
		t.Fatalf("vtt events = %d, want 2", len(res.Events))
	}
	if res.Events[0].StartMS != 1000 || res.Events[0].EndMS != 2500 {
		t.Fatalf("first cue timing wrong: %+v", res.Events[0])
	}
	if res.Events[1].StartMS != 3_603_000 {
		t.Fatalf("hour-form cue wrong: %d", res.Events[1].StartMS)
	}
	if content[res.Events[0].RawStart:res.Events[0].RawEnd] != "cue-1\n00:01.000 --> 00:02.500 align:start\nhello\n" {
		t.Fatalf("vtt raw span wrong: %q", content[res.Events[0].RawStart:res.Events[0].RawEnd])
	}
}

func TestParseVTTHeaderAndNotes(t *testing.T) {
	content := "WEBVTT - demo\n\nNOTE a comment\n\nSTYLE\n::cue { color: red }\n\n" +
		"00:01.000 --> 00:02.000\nok\n"
	res, err := ParseVTT(strings.NewReader(content), "n.vtt")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Events) != 1 {
		t.Fatalf("events after blocks = %d, want 1", len(res.Events))
	}

	missing, err := ParseVTT(strings.NewReader("garbage\n"), "m.vtt")
	if err != nil {
		t.Fatal(err)
	}
	if len(missing.Events) != 0 || len(missing.Faults) != 1 {
		t.Fatalf("missing header must yield one fault, got %+v", missing.Faults)
	}
}

func TestParseVTTBadCueRecovers(t *testing.T) {
	content := "WEBVTT\n\n00:01.000 --> bad\nx\n\n00:03.000 --> 00:04.000\nkept\n"
	res, err := ParseVTT(strings.NewReader(content), "b.vtt")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Events) != 1 || res.Events[0].Text != "kept" {
		t.Fatalf("valid cue after bad cue must survive, got %+v", res.Events)
	}
}

func TestSRTMidnightWrapNotAnOverlap(t *testing.T) {
	// Second cue wraps past midnight; its raw label (00:00:01) is numerically
	// smaller than the previous end but must not be flagged as an overlap.
	content := "1\n23:59:59,000 --> 23:59:59,500\nlast of day\n\n" +
		"2\n00:00:01,000 --> 00:00:02,000\nfirst after midnight\n"
	res, err := ParseSRT(strings.NewReader(content), "wrap.srt")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Events) != 2 {
		t.Fatalf("wrapped cues must both parse: %d", len(res.Events))
	}
	for _, f := range res.Faults {
		if strings.Contains(f.Message, "overlap") {
			t.Fatalf("midnight wrap misreported as overlap: %v", f)
		}
	}
	// A small backwards jump that is NOT a midnight wrap stays an overlap.
	small := "1\n00:00:10,000 --> 00:00:20,000\na\n\n2\n00:00:15,000 --> 00:00:18,000\nb\n"
	r2, err := ParseSRT(strings.NewReader(small), "ov.srt")
	if err != nil {
		t.Fatal(err)
	}
	sawOverlap := false
	for _, f := range r2.Faults {
		if strings.Contains(f.Message, "overlap") {
			sawOverlap = true
		}
	}
	if !sawOverlap {
		t.Fatal("genuine small backwards jump must still be an overlap")
	}
}

func TestTimeParsersRejectBadFields(t *testing.T) {
	if _, ok := parseSRTTime("00:00:60,000"); ok {
		t.Fatal("seconds=60 must be rejected")
	}
	if _, ok := parseVTTTime("00:99.000"); ok {
		t.Fatal("minutes=99 must be rejected")
	}
	if _, ok := parseVTTTime("00:01.00"); ok {
		t.Fatal("two-digit millisecond field must be rejected")
	}
}
