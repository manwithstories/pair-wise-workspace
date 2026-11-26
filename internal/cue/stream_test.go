package cue

import (
	"strings"
	"testing"

	"deliver/internal/model"
)

func TestParseSRT(t *testing.T) {
	in := "1\n00:00:01,000 --> 00:00:02,500\nhello\n\n" +
		"2\n00:00:03,000 --> 00:00:04,000\nworld\n"
	cues, errs := Parse(strings.NewReader(in), false, 0)
	if len(errs) != 0 || len(cues) != 2 {
		t.Fatalf("got %d cues, %d errs", len(cues), len(errs))
	}
	if cues[0].StartMs != 1000 || cues[0].EndMs != 2500 || cues[0].Text != "hello" {
		t.Errorf("bad cue: %+v", cues[0])
	}
	if cues[0].Line != 2 {
		t.Errorf("timestamp line = %d, want 2", cues[0].Line)
	}
}

func TestParseVTT(t *testing.T) {
	in := "WEBVTT\n\n00:01.000 --> 00:02.000\nshort form\n\n" +
		"00:00:05.500 --> 00:00:06.000\nfull form\n"
	cues, errs := Parse(strings.NewReader(in), true, 0)
	if len(errs) != 0 || len(cues) != 2 {
		t.Fatalf("got %d cues, %d errs", len(cues), len(errs))
	}
	if cues[0].StartMs != 1000 || cues[1].StartMs != 5500 {
		t.Errorf("bad times: %+v", cues)
	}
}

func TestBadRecordsSkippedWithLine(t *testing.T) {
	in := "1\n00:00:01,000 --> 00:00:02,000\nok\n\n" +
		"2\nnot-a-timestamp\nbroken\n\n" +
		"3\n00:00:05,000 --> 00:00:04,000\nreversed\n\n" +
		"4\n00:00:06,000 --> 00:00:07,000\nok again\n"
	cues, errs := Parse(strings.NewReader(in), false, 0)
	if len(cues) != 2 || len(errs) != 2 {
		t.Fatalf("got %d cues, %d errs; want 2 and 2", len(cues), len(errs))
	}
	e := errs[0].(*model.Error)
	if e.Line != 6 || !e.Recoverable || e.Stage != model.StageCue {
		t.Errorf("bad error metadata: %+v", e)
	}
}

func TestTruncatedAndOverlong(t *testing.T) {
	// Truncated final record: no timestamp row at all.
	_, errs := Parse(strings.NewReader("1\n00:00:01,000 -->"), false, 0)
	if len(errs) != 1 {
		t.Fatalf("truncated record: got %d errs", len(errs))
	}
	// Overlong record skipped.
	long := "1\n00:00:01,000 --> 00:00:02,000\n" + strings.Repeat("x", 100) + "\n"
	cues, errs := Parse(strings.NewReader(long), false, 64)
	if len(cues) != 0 || len(errs) != 1 {
		t.Fatalf("overlong: got %d cues, %d errs", len(cues), len(errs))
	}
}

func TestSortStableOverlap(t *testing.T) {
	cues := []Cue{
		{Index: 0, StartMs: 1000, EndMs: 3000, Text: "a"},
		{Index: 1, StartMs: 1000, EndMs: 2000, Text: "b"},
		{Index: 2, StartMs: 500, EndMs: 1500, Text: "c"},
		{Index: 3, StartMs: 1000, EndMs: 2000, Text: "d"},
	}
	SortStable(cues)
	got := []string{cues[0].Text, cues[1].Text, cues[2].Text, cues[3].Text}
	want := []string{"c", "b", "d", "a"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
}
