package subtitle

import (
	"strings"
	"testing"

	"mediasync/internal/clock"
)

func testTB() clock.Timebase {
	return clock.Timebase{FPS: clock.Rational{Num: 30000, Den: 1001}}
}

func TestParseSRTValid(t *testing.T) {
	in := "1\n00:00:01,000 --> 00:00:02,500\nhello\n\n" +
		"2\n00:00:03,000 --> 00:00:04,000\nworld\n\n"
	rep := ParseSRT(strings.NewReader(in), testTB())
	if len(rep.Rejected) != 0 {
		t.Fatalf("unexpected rejects: %+v", rep.Rejected)
	}
	if len(rep.Records) != 2 {
		t.Fatalf("got %d records", len(rep.Records))
	}
	r := rep.Records[0]
	if r.StartRaw != "00:00:01,000" || r.EndRaw != "00:00:02,500" {
		t.Fatalf("raw offsets not preserved: %+v", r)
	}
	if r.Start != 30 || r.End != 75 {
		t.Fatalf("frames = %d..%d, want 30..75", r.Start, r.End)
	}
	if r.Source != "srt" || r.Line != 2 {
		t.Fatalf("source/line = %q/%d", r.Source, r.Line)
	}
}

func TestParseSRTInvalidTime(t *testing.T) {
	in := "1\n00:00:61,000 --> 00:00:02,000\nbad seconds\n\n" +
		"2\n00:00:03,000 --> 00:00:04,000\nok\n\n"
	rep := ParseSRT(strings.NewReader(in), testTB())
	if len(rep.Records) != 1 {
		t.Fatalf("got %d records", len(rep.Records))
	}
	if len(rep.Rejected) != 1 || rep.Rejected[0].Line != 2 {
		t.Fatalf("rejects = %+v, want line 2", rep.Rejected)
	}
}

func TestParseSRTTruncated(t *testing.T) {
	in := "1\n00:00:01,000 --> 00:00:02,000\nok\n\n9\n" // trailing index only
	rep := ParseSRT(strings.NewReader(in), testTB())
	if len(rep.Records) != 1 {
		t.Fatalf("got %d records", len(rep.Records))
	}
	if len(rep.Rejected) != 1 || rep.Rejected[0].Line != 5 {
		t.Fatalf("rejects = %+v, want truncated at line 5", rep.Rejected)
	}
}

func TestParseSRTOverlapAndDuplicate(t *testing.T) {
	in := "1\n00:00:01,000 --> 00:00:03,000\na\n\n" +
		"2\n00:00:02,000 --> 00:00:04,000\noverlap, skipped\n\n" +
		"3\n00:00:05,000 --> 00:00:06,000\ndup one\n\n" +
		"4\n00:00:05,000 --> 00:00:06,000\ndup two\n\n"
	rep := ParseSRT(strings.NewReader(in), testTB())
	if len(rep.Records) != 3 {
		t.Fatalf("got %d records, want 3 (dup kept)", len(rep.Records))
	}
	if len(rep.Rejected) != 1 || rep.Rejected[0].Line != 6 {
		t.Fatalf("rejects = %+v, want overlap at line 6", rep.Rejected)
	}
	if !rep.Records[1].Ambiguous || !rep.Records[2].Ambiguous {
		t.Fatal("duplicate timestamps must be marked ambiguous")
	}
	if rep.Records[0].Ambiguous {
		t.Fatal("non-duplicate record marked ambiguous")
	}
}
