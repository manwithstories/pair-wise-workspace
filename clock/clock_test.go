package clock_test

import (
	"testing"

	"archivectl/internal/clock"
	"archivectl/internal/core"
)

func TestUnwrap32BitWrap(t *testing.T) {
	raw := []uint32{0xFFFFFFF0, 0x00000010, 0x00000020}
	got, err := clock.UnwrapTicks(raw)
	if err != nil {
		t.Fatal(err)
	}
	want := []int64{4294967280, 4294967312, 4294967328}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("unwrap[%d]=%d want %d (all=%v)", i, got[i], want[i], got)
		}
	}
	if err := core.CheckMonotonic(got); err != nil {
		t.Fatal(err)
	}
}

func TestVariableBlockRangesDoNotAssumeFixedLength(t *testing.T) {
	blocks, err := clock.CumulativeRanges([]int{3072, 5120, 1024})
	if err != nil {
		t.Fatal(err)
	}
	if blocks[0].StartFrame != 0 || blocks[0].EndFrame != 3072 {
		t.Fatalf("bad first range: %+v", blocks[0])
	}
	if blocks[1].StartFrame != 3072 || blocks[1].EndFrame != 8192 {
		t.Fatalf("bad variable second range: %+v", blocks[1])
	}
	if blocks[2].StartFrame != 8192 || blocks[2].EndFrame != 9216 {
		t.Fatalf("bad third range: %+v", blocks[2])
	}
}

func TestDriftMappingStaysBoundedWithoutCumulativeRounding(t *testing.T) {
	rate := core.MustRat(1003, 1000)
	start := int64(0xFFFFFC00)
	var anchors []clock.Anchor
	for frame := int64(0); frame <= 196608; frame += 4096 {
		tick := uint32(start + core.ScaleFloor(frame, rate))
		anchors = append(anchors, clock.Anchor{CueID: uint32(frame/4096 + 1), Tick: tick, Frame: frame})
	}
	model, normalized, err := clock.EstimateDrift(anchors)
	if err != nil {
		t.Fatal(err)
	}
	if model.TicksPerFrame.N <= 0 || model.TicksPerFrame.D <= 0 {
		t.Fatalf("invalid rate %+v", model.TicksPerFrame)
	}
	for _, a := range normalized {
		if a.FrameError < -1 || a.FrameError > 1 {
			t.Fatalf("rounding/error accumulated at frame %d: mapped=%d error=%d",
				a.WAVFrame, a.MappedFrame, a.FrameError)
		}
	}
}

func TestEstimateRejectsNonMonotonicUnwrappedClock(t *testing.T) {
	// Frames are sorted by EstimateDrift, so construct ticks that remain
	// backwards even after unwrapping.
	anchors := []clock.Anchor{
		{CueID: 1, Tick: 100, Frame: 0},
		{CueID: 2, Tick: 90, Frame: 100},
	}
	_, _, err := clock.EstimateDrift(anchors)
	if err == nil {
		t.Fatal("expected clock estimation error")
	}
}
