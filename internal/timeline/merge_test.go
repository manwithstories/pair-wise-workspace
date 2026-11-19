package timeline

import (
	"fmt"
	"strings"
	"testing"

	"review/internal/clock"
	"review/internal/issues"
	"review/internal/subtitle"
)

func TestParseTimecodeListDayWrap(t *testing.T) {
	r := clock.Rate2997
	var b strings.Builder
	base := clock.Timecode{HH: 23, MM: 59, SS: 58, Drop: true}
	f0, _ := base.ToFrames(r)
	for i := 0; i < 5; i++ {
		tc, _ := clock.FormatTimecode(f0+int64(i)*30, r, true)
		fmt.Fprintf(&b, "%d,%s\n", i, tc)
	}
	b.WriteString("5,25:99:99:99\n") // unconvertible
	recs, iss := ParseTimecodeList([]byte(b.String()), "t.csv", r, true)
	if len(iss) != 1 || iss[0].Kind != issues.TCUnconvertible {
		t.Fatalf("issues: %v", iss)
	}
	if len(recs) != 5 {
		t.Fatalf("recs: %d", len(recs))
	}
	// The list crosses midnight: the last records must sit one day later.
	day := r.FramesPerDay(true)
	if recs[4].AbsFrame < day {
		t.Fatalf("no day wrap applied: %d", recs[4].AbsFrame)
	}
	if recs[4].AbsFrame-recs[3].AbsFrame != 30 {
		t.Fatalf("non-contiguous wrap: %d -> %d", recs[3].AbsFrame, recs[4].AbsFrame)
	}
}

func TestBuildBlocksSources(t *testing.T) {
	r := clock.Rate30
	cues := []subtitle.Cue{
		{Track: "a.srt", ByteOffset: 10, Start: 0.5, End: 2.5},  // frames 15..75
		{Track: "a.srt", ByteOffset: 40, Start: 3.0, End: 3.5},  // frames 90..105
		{Track: "b.vtt", ByteOffset: 70, Start: 1.9, End: 4.2},  // spans boundary
	}
	tcs := []TCRecord{{Seq: 0, AbsFrame: 0, ByteOffset: 0}, {Seq: 1, AbsFrame: 60, ByteOffset: 20}}
	blocks := BuildBlocks(0, 120, 60, cues, tcs, r)
	if len(blocks) != 2 {
		t.Fatalf("blocks: %d", len(blocks))
	}
	if len(blocks[0].Spans) != 2 || len(blocks[1].Spans) != 3 {
		t.Fatalf("spans: %d %d", len(blocks[0].Spans), len(blocks[1].Spans))
	}
	// The vtt cue (frames 57..126) crosses the block boundary at 60.
	found := false
	for _, n := range blocks[0].Notes {
		if strings.Contains(n, "spans block boundary") {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing boundary note: %v", blocks[0].Notes)
	}
	if len(blocks[0].Timecodes) != 1 || len(blocks[1].Timecodes) != 1 {
		t.Fatalf("timecodes not mapped: %+v", blocks)
	}
}
