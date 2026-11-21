package captions

import (
	"strings"
	"testing"

	"mediacheck/internal/fault"
	"mediacheck/internal/timeline"
)

func TestSRTBasic(t *testing.T) {
	in := "1\n00:00:01,000 --> 00:00:02,500\nhello\n\n2\n00:00:03,000 --> 00:00:04,000\nworld\n"
	res := ParseSRT(strings.NewReader(in))
	if res.Faults.Len() != 0 {
		t.Fatalf("unexpected faults: %v", res.Faults.Errs)
	}
	if len(res.Cues) != 2 {
		t.Fatalf("want 2 cues, got %d", len(res.Cues))
	}
	c := res.Cues[0]
	if c.Seq != 1 || c.Start != timeline.NanosPerSecond || c.End != 2500*1_000_000 {
		t.Errorf("cue0: %+v", c)
	}
	if c.RawStart != "00:00:01,000" || c.RawEnd != "00:00:02,500" {
		t.Errorf("raw times not preserved: %+v", c)
	}
}

func TestSRTTruncatedBlock(t *testing.T) {
	in := "1\n00:00:01,000 --> 00:00:02,000\nok\n\n2\n" // trailing index, no time line
	res := ParseSRT(strings.NewReader(in))
	if len(res.Cues) != 1 {
		t.Fatalf("want 1 cue, got %d", len(res.Cues))
	}
	if res.Faults.Len() == 0 {
		t.Fatal("truncated block must be reported")
	}
	for _, e := range res.Faults.Errs {
		if !e.Recoverable {
			t.Errorf("truncation should be recoverable: %v", e)
		}
		if e.Offset < 0 {
			t.Errorf("fault missing offset: %v", e)
		}
	}
}

func TestSRTReversedAndIllegalTime(t *testing.T) {
	in := "1\n00:00:05,000 --> 00:00:02,000\nreversed\n\n2\n00:00:99,000 --> 00:00:99,500\nbad\n"
	res := ParseSRT(strings.NewReader(in))
	// Cue 1 kept with reversed-time fault; cue 2 dropped with fault.
	if len(res.Cues) != 1 {
		t.Fatalf("want 1 cue, got %d", len(res.Cues))
	}
	if res.Faults.Len() < 2 {
		t.Fatalf("want >=2 faults, got %d: %v", res.Faults.Len(), res.Faults.Errs)
	}
	cats := res.Faults.ByCategory()
	if cats[fault.CatCaption] < 2 {
		t.Errorf("faults not categorized as caption: %v", cats)
	}
}

func TestVTTBasic(t *testing.T) {
	in := "WEBVTT\n\n00:00:01.000 --> 00:00:02.000\nhello\n\ncue-id\n00:01:00.000 --> 00:01:02.000 align:center\nworld\n"
	res := ParseVTT(strings.NewReader(in))
	if res.Faults.Len() != 0 {
		t.Fatalf("unexpected faults: %v", res.Faults.Errs)
	}
	if len(res.Cues) != 2 {
		t.Fatalf("want 2 cues, got %d", len(res.Cues))
	}
	if res.Cues[1].Start != 60*timeline.NanosPerSecond {
		t.Errorf("cue1 start: %d", res.Cues[1].Start)
	}
	if res.Cues[1].RawEnd != "00:01:02.000" {
		t.Errorf("settings leaked into raw end: %q", res.Cues[1].RawEnd)
	}
}

func TestVTTShortFormAndMissingHeader(t *testing.T) {
	in := "WEBVTT\n\n01:02.500 --> 01:03.000\nshort form\n"
	res := ParseVTT(strings.NewReader(in))
	if res.Faults.Len() != 0 || len(res.Cues) != 1 {
		t.Fatalf("short form: cues=%d faults=%v", len(res.Cues), res.Faults.Errs)
	}
	if res.Cues[0].Start != 62*timeline.NanosPerSecond+500*1_000_000 {
		t.Errorf("short form start: %d", res.Cues[0].Start)
	}
	bad := ParseVTT(strings.NewReader("00:00:01.000 --> 00:00:02.000\nx\n"))
	if !bad.Faults.Fatal() {
		t.Error("missing WEBVTT header should be fatal")
	}
}

func TestOutOfRangeAndOverlap(t *testing.T) {
	// 49h exceeds the 48h timeline bound.
	in := "WEBVTT\n\n49:00:00.000 --> 49:00:01.000\ntoo far\n"
	res := ParseVTT(strings.NewReader(in))
	if res.Faults.Len() == 0 {
		t.Fatal("out-of-range cue must be reported")
	}
	// Overlapping cues are legal to parse but both must be retained.
	in2 := "WEBVTT\n\n00:00:01.000 --> 00:00:03.000\na\n\n00:00:02.000 --> 00:00:04.000\nb\n"
	res2 := ParseVTT(strings.NewReader(in2))
	if len(res2.Cues) != 2 {
		t.Fatalf("overlapping cues dropped: %d", len(res2.Cues))
	}
}

func TestCueBlockCap(t *testing.T) {
	var sb strings.Builder
	sb.WriteString("WEBVTT\n\n00:00:01.000 --> 00:00:02.000\n")
	sb.WriteString(strings.Repeat("x", MaxCueBytes))
	res := ParseVTT(strings.NewReader(sb.String()))
	if res.Faults.Len() == 0 {
		t.Fatal("oversize block must be reported")
	}
}
