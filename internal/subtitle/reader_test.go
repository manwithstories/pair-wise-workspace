package subtitle

import (
	"context"
	"strings"
	"testing"

	"archivetl/internal/clock"
	"archivetl/internal/diag"
)

func collect(t *testing.T, input string) ([]Record, *diag.Collector) {
	t.Helper()
	coll := diag.NewCollector(0)
	p := NewParser(strings.NewReader(input), "sample.srt", coll)
	var got []Record
	err := p.Parse(context.Background(), func(r Record) error {
		got = append(got, r)
		return nil
	})
	if err != nil {
		t.Fatalf("parse returned %v", err)
	}
	return got, coll
}

func TestSRTBasic(t *testing.T) {
	input := "\uFEFF1\r\n00:00:01,000 --> 00:00:02,500\r\nhello\r\n\r\n" +
		"2\r\n00:01:00,000 --> 00:01:01,000\r\nworld\r\n"
	got, coll := collect(t, input)
	if len(got) != 2 || coll.Len() != 0 {
		t.Fatalf("records=%d errors=%d", len(got), coll.Len())
	}
	if got[0].Start != clock.MillisToTick(1000) || got[0].End != clock.MillisToTick(2500) {
		t.Fatalf("timing wrong: %+v", got[0])
	}
	if got[0].Text != "hello" || got[0].Offset != int64(len("\uFEFF1\r\n")) {
		t.Fatalf("text/offset wrong: %+v offset=%d", got[0], got[0].Offset)
	}
	if got[0].Sequence != 1 || got[1].Sequence != 2 {
		t.Fatal("sequence numbering wrong")
	}
}

func TestVTT(t *testing.T) {
	input := "WEBVTT\n\ncue-id-7\n00:00:01.000 --> 00:00:02.000 line:90% position:10%\nsubtitle A\n\n" +
		"00:05.000 --> 00:06.000\nno identifier\n"
	got, coll := collect(t, input)
	if len(got) != 2 || coll.Len() != 0 {
		t.Fatalf("records=%d errors=%d", len(got), coll.Len())
	}
	if got[0].Text != "subtitle A" {
		t.Fatalf("vtt text=%q", got[0].Text)
	}
	if got[1].Start != clock.MillisToTick(5000) {
		t.Fatal("MM:SS VTT stamp not parsed")
	}
}

func TestMissingEndTime(t *testing.T) {
	input := "1\n00:00:01,000 --> \nlonely cue\n\n2\n00:00:02,000 --> 00:00:03,000\nok\n"
	got, coll := collect(t, input)
	if len(got) != 2 {
		t.Fatalf("expected recoverable cue, got %d", len(got))
	}
	if got[0].Start != got[0].End {
		t.Fatal("missing end should produce zero-duration cue")
	}
	if coll.Len() != 1 || coll.All()[0].Offset < 0 || !coll.All()[0].Recoverable {
		t.Fatalf("expected one recoverable offset error, got %+v", coll.All())
	}
}

func TestTruncatedUTF8(t *testing.T) {
	input := "1\n00:00:01,000 --> 00:00:02,000\n" + "abc\xff\xfe" + "def\n\n2\n00:00:03,000 --> 00:00:04,000\nfine\n"
	got, coll := collect(t, input)
	if len(got) != 2 || coll.Len() != 1 {
		t.Fatalf("records=%d errors=%d", len(got), coll.Len())
	}
	if !strings.Contains(got[0].Text, "�") {
		t.Fatalf("invalid bytes not sanitised: %q", got[0].Text)
	}
	if got[1].Text != "fine" {
		t.Fatal("parser lost sync after invalid UTF-8")
	}
}

func TestOversizedBlock(t *testing.T) {
	big := strings.Repeat("x", DefaultMaxBlockBytes+5000)
	input := "1\n00:00:01,000 --> 00:00:02,000\n" + big + "\n\n" +
		"2\n00:00:03,000 --> 00:00:04,000\nnext\n"
	got, coll := collect(t, input)
	if len(got) != 2 {
		t.Fatalf("records=%d, oversized block must not kill the stream", len(got))
	}
	if len(got[0].Text) > DefaultMaxBlockBytes {
		t.Fatalf("block cap breached: %d bytes", len(got[0].Text))
	}
	if coll.Len() != 1 || got[1].Text != "next" {
		t.Fatalf("truncation not reported or sync lost: errs=%d", coll.Len())
	}
}

func TestBadHeaderAndBareCR(t *testing.T) {
	input := "garbage line without timing\n\n1\r00:00:01,000 --> 00:00:02,000\rcr cue\r\n"
	got, coll := collect(t, input)
	if len(got) != 1 || got[0].Text != "cr cue" {
		t.Fatalf("bare CR parsing failed: %+v", got)
	}
	if coll.Len() != 1 {
		t.Fatalf("headerless block should be reported, errs=%d", coll.Len())
	}
}

func TestCancelStopsParse(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p := NewParser(strings.NewReader("1\n00:00:01,000 --> 00:00:02,000\nx\n"), "x.srt", nil)
	if err := p.Parse(ctx, func(Record) error { return nil }); err == nil {
		t.Fatal("cancelled context did not stop the parser")
	}
}
