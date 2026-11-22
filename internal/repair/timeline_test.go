package repair

import (
	"testing"

	"mediatool/internal/clock"
	"mediatool/internal/fixture"
	"mediatool/internal/ts"
)

func recoverFixture(spec fixture.FaultSpec, durMs int) Timeline {
	s := fixture.Generate(42, durMs, spec)
	res := ts.Parse(s.Data)
	a := ts.NewPSIAssembler()
	var psiFaults clock.Faults
	for _, p := range res.Packets {
		_, f := a.Feed(p)
		psiFaults = append(psiFaults, f...)
	}
	psiFaults = append(psiFaults, a.Flush()...)
	return Recover(res, fixture.PIDVideo, 2000, psiFaults)
}

func TestRecoverClean(t *testing.T) {
	tl := recoverFixture(fixture.FaultSpec{}, 6000)
	if len(tl.Frames) != 150 {
		t.Fatalf("frames = %d, want 150", len(tl.Frames))
	}
	for _, s := range tl.Segments {
		if !s.Complete {
			t.Fatalf("segment %d incomplete: %v", s.ID, s.Faults)
		}
	}
	// 单调时间轴
	for i := 1; i < len(tl.Frames); i++ {
		if tl.Frames[i].PTS <= tl.Frames[i-1].PTS {
			t.Fatalf("non-monotonic at %d", i)
		}
	}
}

func TestRecoverPCRWrap(t *testing.T) {
	tl := recoverFixture(fixture.FaultSpec{PCRWrap: true}, 8000)
	if !hasKind(tl.Faults, clock.FaultPCRWrap) {
		t.Fatal("pcr-wrap fault not recorded")
	}
	for i := 1; i < len(tl.Frames); i++ {
		if tl.Frames[i].PTS <= tl.Frames[i-1].PTS {
			t.Fatalf("timeline regresses at frame %d", i)
		}
	}
	for _, s := range tl.Segments {
		if !s.Complete {
			t.Fatalf("wrap should be recovered, segment %d incomplete", s.ID)
		}
	}
}

func TestRecoverCCGapInsertsFrame(t *testing.T) {
	tl := recoverFixture(fixture.FaultSpec{CCGap: true}, 6000)
	rec := 0
	for _, f := range tl.Frames {
		if f.Recovered {
			rec++
		}
	}
	if rec != 1 {
		t.Fatalf("recovered frames = %d, want 1", rec)
	}
	if len(tl.Frames) != 150 { // 丢 1 帧 + 恢复 1 帧
		t.Fatalf("frames = %d, want 150", len(tl.Frames))
	}
}

func TestRecoverCCConflictMarksSegment(t *testing.T) {
	tl := recoverFixture(fixture.FaultSpec{CCConflict: true}, 6000)
	incomplete := 0
	for _, s := range tl.Segments {
		if !s.Complete {
			incomplete++
		}
	}
	if incomplete == 0 {
		t.Fatal("cc-conflict must mark a segment incomplete")
	}
}

func TestRecoverTruncatedPSIPropagates(t *testing.T) {
	tl := recoverFixture(fixture.FaultSpec{TruncatedPSI: true}, 6000)
	if !hasKind(tl.Faults, clock.FaultPSIIncomplete) {
		t.Fatal("truncated-psi fault not propagated to timeline")
	}
	incomplete := 0
	for _, s := range tl.Segments {
		if !s.Complete {
			incomplete++
		}
	}
	if incomplete == 0 {
		t.Fatal("truncated-psi must propagate to segment completeness")
	}
}

func TestRecoverDeterministic(t *testing.T) {
	a := recoverFixture(fixture.FaultSpec{PCRWrap: true, CCGap: true}, 6000)
	b := recoverFixture(fixture.FaultSpec{PCRWrap: true, CCGap: true}, 6000)
	if len(a.Frames) != len(b.Frames) || len(a.Segments) != len(b.Segments) {
		t.Fatal("nondeterministic structure")
	}
	for i := range a.Frames {
		if a.Frames[i] != b.Frames[i] {
			t.Fatalf("frame %d differs", i)
		}
	}
}

func hasKind(fs clock.Faults, k clock.FaultKind) bool {
	for _, f := range fs {
		if f.Kind == k {
			return true
		}
	}
	return false
}
