package caption

import (
	"strings"
	"testing"

	"avcore/internal/adapter"
	"avcore/internal/clock"
)

func TestParseSRTValid(t *testing.T) {
	feed := "1\n00:00:01,000 --> 00:00:02,500\nhello\n\n" +
		"2\n00:00:03,000 --> 00:00:04,000\nworld\n"
	res := ParseSRT(strings.NewReader(feed), MaxCaptionBytes)
	if res.Fatal != nil {
		t.Fatalf("unexpected fatal: %v", res.Fatal)
	}
	if len(res.Cues) != 2 {
		t.Fatalf("cues = %d, want 2", len(res.Cues))
	}
	if res.Cues[0].StartTicks != clock.TicksPerSecond ||
		res.Cues[0].EndTicks != clock.Millis(2500) {
		t.Errorf("first cue ticks wrong: %+v", res.Cues[0])
	}
	if res.Cues[0].Text != "hello" || res.Cues[0].SourceLine != 1 {
		t.Errorf("first cue payload/source wrong: %+v", res.Cues[0])
	}
	if len(res.BadRecords) != 0 {
		t.Errorf("unexpected bad records: %+v", res.BadRecords)
	}
}

func TestParseSRTBadRecords(t *testing.T) {
	// Bad timestamp on cue 2, overlapping cue 3. Records are blank-separated
	// so each bad record is isolated and recoverable.
	feed := strings.Join([]string{
		"1", "00:00:01,000 --> 00:00:02,000", "good", "",
		"2", "00:00:99,000 --> 00:00:03,000", "badtime", "",
		"3", "00:00:01,500 --> 00:00:06,000", "overlap", "",
	}, "\n")
	res := ParseSRT(strings.NewReader(feed), MaxCaptionBytes)
	if res.Fatal != nil {
		t.Fatalf("unexpected fatal: %v", res.Fatal)
	}
	if len(res.Cues) != 2 {
		t.Fatalf("recoverable cues = %d, want 2", len(res.Cues))
	}
	if len(res.BadRecords) != 2 {
		t.Fatalf("bad records = %d, want 2: %+v", len(res.BadRecords), res.BadRecords)
	}
	wantCodes := []adapter.ErrorCode{
		adapter.CodeBadTimestamp, adapter.CodeOverlap,
	}
	for i, b := range res.BadRecords {
		if b.Code != wantCodes[i] {
			t.Errorf("record %d code = %s, want %s", i, b.Code, wantCodes[i])
		}
		if b.Line <= 0 || b.Offset < 0 {
			t.Errorf("record %d missing location: %+v", i, b)
		}
	}
	// Every bad record must carry a byte offset that points at its line.
	if res.BadRecords[0].Offset == res.BadRecords[1].Offset {
		t.Error("bad record offsets must differ per line")
	}
}

func TestParseSRTTruncatedPayload(t *testing.T) {
	// Timing line followed immediately by EOF with no text payload.
	res := ParseSRT(strings.NewReader("1\n00:00:01,000 --> 00:00:02,000\n"), MaxCaptionBytes)
	if len(res.BadRecords) != 1 || res.BadRecords[0].Code != adapter.CodeTruncated {
		t.Errorf("truncated payload not reported: %+v", res.BadRecords)
	}
}

func TestParseSRTEndNotAfterStart(t *testing.T) {
	feed := "1\n00:00:02,000 --> 00:00:01,000\nx\n\n2\n00:00:03,000 --> 00:00:04,000\ny\n"
	res := ParseSRT(strings.NewReader(feed), MaxCaptionBytes)
	if len(res.Cues) != 1 || res.Cues[0].Text != "y" {
		t.Errorf("recovery after bad range failed: %+v", res.Cues)
	}
	if len(res.BadRecords) != 1 || res.BadRecords[0].Code != adapter.CodeBadTimestamp {
		t.Errorf("want one bad timestamp, got %+v", res.BadRecords)
	}
}

func TestParseSRTTruncatedStream(t *testing.T) {
	// Index present but the timing line itself is cut off by EOF.
	res := ParseSRT(strings.NewReader("1\n"), MaxCaptionBytes)
	if len(res.BadRecords) != 1 || res.BadRecords[0].Code != adapter.CodeTruncated {
		t.Errorf("missing timing line not reported as truncated: %+v", res.BadRecords)
	}
}

func TestParseSRTMalformedTiming(t *testing.T) {
	// A complete line that simply cannot be parsed as a time range.
	res := ParseSRT(strings.NewReader("1\n00:00:01,000 --> \nbody\n\n2\n00:00:03,000 --> 00:00:04,000\nok\n"),
		MaxCaptionBytes)
	if len(res.Cues) != 1 || res.Cues[0].Text != "ok" {
		t.Errorf("parser must resync after a malformed timing line: %+v", res.Cues)
	}
	if len(res.BadRecords) != 1 || res.BadRecords[0].Code != adapter.CodeBadTimestamp {
		t.Errorf("malformed timing not reported: %+v", res.BadRecords)
	}
}

func TestParseVTTValid(t *testing.T) {
	feed := "WEBVTT\n\n" +
		"intro-1\n00:01.000 --> 00:02.500\nshort form\n\n" +
		"00:00:03.000 --> 00:00:04.000 line:0 position:20%\nlong form\n"
	res := ParseWebVTT(strings.NewReader(feed), MaxCaptionBytes)
	if res.Fatal != nil {
		t.Fatalf("unexpected fatal: %v", res.Fatal)
	}
	if len(res.Cues) != 2 {
		t.Fatalf("cues = %d, want 2: %+v", len(res.Cues), res.BadRecords)
	}
	if res.Cues[0].StartTicks != clock.TicksPerSecond ||
		res.Cues[1].EndTicks != clock.Millis(4000) {
		t.Errorf("vtt timestamp parsing wrong: %+v", res.Cues)
	}
}

func TestParseVTTBlocksAndBadRecords(t *testing.T) {
	feed := strings.Join([]string{
		"WEBVTT", "",
		"NOTE this is a comment", "spanning two lines", "",
		"STYLE", "::cue { color: red }", "",
		"cue-1", "00:00:01.000 --> 00:00:02.000", "ok", "",
		"cue-2", "bad --> 00:00:03.000", "skipped", "",
	}, "\n")
	res := ParseWebVTT(strings.NewReader(feed), MaxCaptionBytes)
	if res.Fatal != nil {
		t.Fatalf("unexpected fatal: %v", res.Fatal)
	}
	if len(res.Cues) != 1 || res.Cues[0].Text != "ok" {
		t.Errorf("block skipping / recovery failed: %+v", res.Cues)
	}
	if len(res.BadRecords) != 1 || res.BadRecords[0].Code != adapter.CodeBadTimestamp {
		t.Errorf("want one bad vtt timestamp, got %+v", res.BadRecords)
	}
}

func TestParseVTTRejectsNonVTT(t *testing.T) {
	res := ParseWebVTT(strings.NewReader("1\n00:00:01,000 --> 00:00:02,000\nx\n"), MaxCaptionBytes)
	if res.Fatal == nil || res.Fatal.Code != adapter.CodeUnsupported {
		t.Errorf("non-VTT feed must be fatal, got %+v", res.Fatal)
	}
}

func TestCaptionOversizeRejected(t *testing.T) {
	big := strings.Repeat("x", 4096)
	res := ParseSRT(strings.NewReader(big), 1024)
	if res.Fatal == nil || res.Fatal.Code != adapter.CodeOversize {
		t.Errorf("oversize feed must be rejected, got %+v", res.Fatal)
	}
}

func TestBadRecordLocationsPointAtLines(t *testing.T) {
	feed := "1\n00:00:01,000 --> 00:00:02,000\na\n\n2\n00:00:03,000 --> 00:00:04,000\nb\n"
	res := ParseSRT(strings.NewReader(feed), MaxCaptionBytes)
	for _, c := range res.Cues {
		if c.SourceLine < 1 {
			t.Errorf("cue without source line: %+v", c)
		}
	}
}
