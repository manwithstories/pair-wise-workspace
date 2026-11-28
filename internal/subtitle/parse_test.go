package subtitle

import (
	"strings"
	"testing"

	"presspack/internal/core"
)

const goodSRT = "\ufeff1\r\n00:00:01,000 --> 00:00:03,500\r\nHello world\r\n\r\n2\r\n00:00:04,000 --> 00:00:05,000\r\nSecond line\ncontinued\r\n\r\n"

func TestParseSRTWithBOMAndCRLF(t *testing.T) {
	cues, faults := Parse([]byte(goodSRT))
	if len(faults) != 0 {
		t.Fatalf("unexpected faults: %v", faults)
	}
	if len(cues) != 2 {
		t.Fatalf("got %d cues, want 2", len(cues))
	}
	if cues[0].Start.Millis() != 1000 || cues[0].End.Millis() != 3500 {
		t.Errorf("cue0 = %v..%v", cues[0].Start, cues[0].End)
	}
	if cues[1].Text != "Second line\ncontinued" {
		t.Errorf("cue1 text = %q", cues[1].Text)
	}
}

func TestParseWebVTT(t *testing.T) {
	vtt := "WEBVTT - demo\n\nNOTE this is ignored\n\ncue-1\n00:00:02.000 --> 00:00:04.000 align:center\nHi\n\n00:05.000 --> 00:06.500\nShort form\n"
	cues, faults := Parse([]byte(vtt))
	if len(faults) != 0 {
		t.Fatalf("unexpected faults: %v", faults)
	}
	if len(cues) != 2 {
		t.Fatalf("got %d cues, want 2", len(cues))
	}
	if cues[1].Start.Millis() != 5000 || cues[1].End.Millis() != 6500 {
		t.Errorf("cue1 = %v..%v", cues[1].Start, cues[1].End)
	}
}

func TestBadCueIsolatedWithOffset(t *testing.T) {
	doc := "1\n00:00:01,000 --> 00:00:02,000\ngood\n\n" +
		"2\nnot-a-timeline\nbad payload\n\n" +
		"3\n00:00:03,000 --> 00:00:04,000\nalso good\n"
	cues, faults := Parse([]byte(doc))
	if len(cues) != 2 {
		t.Fatalf("got %d cues, want 2 (bad cue must be isolated)", len(cues))
	}
	bad := faults.OfKind(core.FaultBadCue)
	if len(bad) != 1 {
		t.Fatalf("want 1 bad-cue fault, got %v", faults)
	}
	wantOff := int64(strings.Index(doc, "not-a-timeline")) - 2 // block starts at index line "2\n"
	if bad[0].Offset != wantOff {
		t.Errorf("fault offset = %d, want %d", bad[0].Offset, wantOff)
	}
	if !bad[0].Recoverable {
		t.Error("bad cue must be recoverable")
	}
}

func TestTruncatedTailKeepsValidCues(t *testing.T) {
	doc := "1\n00:00:01,000 --> 00:00:02,000\nkept\n\n" +
		"2\n00:00:03,000 --> 00:00:04,000\n" // text never arrived
	cues, faults := Parse([]byte(doc))
	if len(cues) != 1 || cues[0].Text != "kept" {
		t.Fatalf("cues = %+v", cues)
	}
	if n := len(faults.OfKind(core.FaultTruncatedTail)); n != 1 {
		t.Fatalf("want 1 truncated-tail fault, got %v", faults)
	}
}

func TestTruncatedMidTimeline(t *testing.T) {
	doc := "1\n00:00:01,000 --> 00:00:02,000\nkept\n\n2\n00:00:03,0"
	cues, faults := Parse([]byte(doc))
	if len(cues) != 1 {
		t.Fatalf("cues = %+v", cues)
	}
	if len(faults.OfKind(core.FaultTruncatedTail)) != 1 {
		t.Fatalf("faults = %v", faults)
	}
}

func TestCropShiftsAndClamps(t *testing.T) {
	cues := []Cue{
		{Start: core.FromMillis(500), End: core.FromMillis(2500), Text: "a"},
		{Start: core.FromMillis(3000), End: core.FromMillis(3500), Text: "b"},
		{Start: core.FromMillis(9000), End: core.FromMillis(9500), Text: "c"},
	}
	got := Crop(cues, core.FromMillis(2000), core.FromMillis(4000))
	if len(got) != 2 {
		t.Fatalf("got %d cues, want 2", len(got))
	}
	if got[0].Start.Millis() != 0 || got[0].End.Millis() != 500 {
		t.Errorf("clamped cue0 = %v..%v", got[0].Start, got[0].End)
	}
	if got[1].Start.Millis() != 1000 || got[1].End.Millis() != 1500 {
		t.Errorf("shifted cue1 = %v..%v", got[1].Start, got[1].End)
	}
}
