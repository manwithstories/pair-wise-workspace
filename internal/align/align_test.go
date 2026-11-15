package align

import (
	"testing"

	"dashpack/internal/errs"
	"dashpack/internal/source"
)

func mkRep(id string, timescale, gopMs int64, segs ...source.Segment) source.Rep {
	return source.Rep{
		LadderRep: source.LadderRep{ID: id, ContentType: "video",
			Timescale: timescale, NominalMs: gopMs},
		Segments: segs,
	}
}

func mkSeg(index, dur, offset int64, present bool) source.Segment {
	return source.Segment{
		LadderSegment: source.LadderSegment{
			Index: index, File: "seg.m4s", DurationMs: dur, OffsetMs: offset},
		Present: present,
	}
}

func fullSegs(n int, dur int64) []source.Segment {
	var out []source.Segment
	for i := 1; i <= n; i++ {
		out = append(out, mkSeg(int64(i), dur, 0, true))
	}
	return out
}

func TestTolerantShiftDropsLeadingPartial(t *testing.T) {
	a := mkRep("a", 90000, 2000, fullSegs(3, 2000)...)
	b := mkRep("b", 90000, 2000,
		mkSeg(1, 2000, 37, true), mkSeg(2, 2000, 0, true), mkSeg(3, 2000, 0, true))
	res, err := Align([]source.Rep{a, b}, PolicyTolerant)
	if err != nil {
		t.Fatalf("tolerant align: %v", err)
	}
	if res.CommonStartMs != 0 {
		t.Fatalf("common start = %d, want 0", res.CommonStartMs)
	}
	rb := res.Reps[1]
	if len(rb.Repairs) != 1 || rb.Repairs[0].Kind != "shift" {
		t.Fatalf("repairs = %+v, want one shift", rb.Repairs)
	}
	if rb.Segments[0].StartMs != 0 {
		t.Fatalf("first start = %d, want 0", rb.Segments[0].StartMs)
	}
	if rb.MaxDeviationTicks != 0 {
		t.Fatalf("max deviation = %d ticks, want 0", rb.MaxDeviationTicks)
	}
}

func TestTolerantFillGapCopiesPrevious(t *testing.T) {
	segs := fullSegs(4, 2000)
	segs[1].Present = false
	r := mkRep("a", 90000, 2000, segs...)
	res, err := Align([]source.Rep{r}, PolicyTolerant)
	if err != nil {
		t.Fatalf("tolerant align: %v", err)
	}
	rr := res.Reps[0]
	if len(rr.Segments) != 4 {
		t.Fatalf("segments = %d, want 4", len(rr.Segments))
	}
	if rr.Segments[1].FilledFrom == "" {
		t.Fatalf("gap segment not filled: %+v", rr.Segments[1])
	}
	if rr.Segments[1].StartMs != 2000 || rr.Segments[1].DurationMs != 2000 {
		t.Fatalf("filled segment misplaced: %+v", rr.Segments[1])
	}
}

func TestStrictRejectsOffsetGapAndPartial(t *testing.T) {
	segs := fullSegs(4, 2000)
	segs[0].OffsetMs = 37
	segs[2].Present = false
	segs[3].DurationMs = 1000
	r := mkRep("a", 90000, 2000, segs...)
	_, err := Align([]source.Rep{r}, PolicyStrict)
	if err == nil {
		t.Fatal("strict align should fail")
	}
	ce, ok := err.(*errs.Coded)
	if !ok || ce.Exit != errs.ExitAlignment {
		t.Fatalf("err = %v, want coded exit 3", err)
	}
	msg := err.Error()
	for _, want := range []string{"offset", "missing", "duration"} {
		if !contains(msg, want) {
			t.Fatalf("problem list missing %q: %s", want, msg)
		}
	}
}

func TestDeviationTicksPerTimescale(t *testing.T) {
	// 音频 timescale 48000：1 tick ≈ 0.0208ms；37ms 偏移应产生大量 tick。
	segs := fullSegs(2, 2000)
	segs[0].OffsetMs = 37
	r := mkRep("a", 48000, 2000, segs...)
	_, err := Align([]source.Rep{r}, PolicyStrict)
	if err == nil {
		t.Fatal("strict should fail on 37ms offset at 48000 timescale")
	}
}

func TestTolerantKeepsPartialDuration(t *testing.T) {
	segs := fullSegs(3, 2000)
	segs[2].DurationMs = 1000 // 末片半时长
	r := mkRep("a", 90000, 2000, segs...)
	res, err := Align([]source.Rep{r}, PolicyTolerant)
	if err != nil {
		t.Fatalf("tolerant align: %v", err)
	}
	rr := res.Reps[0]
	if rr.Segments[2].DurationMs != 1000 {
		t.Fatalf("partial duration rewritten: %+v", rr.Segments[2])
	}
	if rr.TotalMs != 5000 {
		t.Fatalf("total = %d, want 5000", rr.TotalMs)
	}
	if len(rr.MisalignedSegments) != 0 {
		t.Fatalf("misaligned = %v, want none", rr.MisalignedSegments)
	}
}

func TestUnknownPolicy(t *testing.T) {
	r := mkRep("a", 90000, 2000, fullSegs(1, 2000)...)
	if _, err := Align([]source.Rep{r}, "loose"); err == nil {
		t.Fatal("unknown policy should fail")
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
