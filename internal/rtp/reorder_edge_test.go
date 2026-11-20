package rtp

import (
	"testing"

	"github.com/mediamend/mediamend/internal/fault"
)

func TestExtendRollover(t *testing.T) {
	ext, wrapped := Extend(65534, 65534, 0)
	if !wrapped || ext != 65536 {
		t.Fatalf("ext=%d wrapped=%v", ext, wrapped)
	}
	ext, wrapped = Extend(65536, 0, 3)
	if wrapped || ext != 65539 {
		t.Fatalf("ext=%d wrapped=%v", ext, wrapped)
	}
	ext, wrapped = Extend(10, 10, 7)
	if wrapped || ext != 7 {
		t.Fatalf("small backwards reorder: ext=%d wrapped=%v", ext, wrapped)
	}
}

func TestMemoryBoundWindow(t *testing.T) {
	r, _ := New(Config{Camera: "cam1", Window: 4, MaxBytes: 100})
	for i := 0; i < 200; i++ {
		r.Push(Frame{Camera: "cam1", FrameSeq: uint64(i), RTPSeq: uint16(i * 2), Size: 10})
	}
	res := r.Finish()
	if res.PeakBuffered > 4 {
		t.Fatalf("peak buffered=%d exceeds window 4", res.PeakBuffered)
	}
	if len(res.Ordered) != 200 {
		t.Fatalf("ordered=%d want 200", len(res.Ordered))
	}
	if res.MissingCount() < 100 {
		t.Fatalf("missing=%d want at least the skipped odds", res.MissingCount())
	}
}

func TestResetOnZeroAfterLargeSeq(t *testing.T) {
	r, _ := New(Config{Camera: "cam1"})
	r.Push(frame(1, 4000))
	r.Push(frame(2, 0))
	res := r.Finish()
	if res.Resets != 1 {
		t.Fatalf("resets=%d want 1 (faults=%v)", res.Resets, res.Faults)
	}
	if len(res.Ordered) != 2 {
		t.Fatalf("ordered=%d want 2", len(res.Ordered))
	}
}

func TestNotResetForSmallBackjump(t *testing.T) {
	r, _ := New(Config{Camera: "cam1"})
	r.Push(frame(1, 500))
	r.Push(frame(2, 480))
	res := r.Finish()
	if res.Resets != 0 {
		t.Fatalf("small backwards jump must not be a reset: %+v", res)
	}
}

func TestWrapWithInWindowPreviousEpochFrame(t *testing.T) {
	r, _ := New(Config{Camera: "cam1"})
	// 65535 is missing at the rollover; 0 and 1 arrive first. The late
	// 65535 still fits the reorder window and must be inserted in order.
	for _, s := range []uint64{65533, 65534, 0, 1, 65535} {
		r.Push(frame(s, s))
	}
	res := r.Finish()
	want := []uint16{65533, 65534, 65535, 0, 1}
	if got := seqOf(res.Ordered); len(got) != len(want) {
		t.Fatalf("order=%v want %v", got, want)
	} else {
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("order=%v want %v", got, want)
			}
		}
	}
	if len(res.Gaps) != 0 || len(res.Late) != 0 {
		t.Fatalf("in-window rollover frame must not create gaps/late: gaps=%+v late=%+v", res.Gaps, res.Late)
	}
	if res.Faults.Count(fault.KindSequenceWrap) != 1 {
		t.Fatalf("faults=%v", res.Faults)
	}
}

func TestLatePreviousEpochFrameOutsideWindow(t *testing.T) {
	r, _ := New(Config{Camera: "cam1", Window: 1})
	for _, s := range []uint64{65534, 0, 1, 2} {
		r.Push(frame(s, s))
	}
	// 65535 arrives only after the window was forced past its slot.
	r.Push(frame(99, 65535))
	res := r.Finish()
	if len(res.Late) != 1 || res.Late[0].ExtSeq != 65535 {
		t.Fatalf("late=%+v", res.Late)
	}
	if res.Faults.Count(fault.KindLateFrame) != 1 {
		t.Fatalf("faults=%v", res.Faults)
	}
}

func TestGapAfterSeqRecorded(t *testing.T) {
	r, _ := New(Config{Camera: "cam1", Window: 2})
	r.Push(frame(10, 10))
	r.Push(frame(11, 11))
	r.Push(frame(12, 14))
	r.Push(frame(13, 15))
	res := r.Finish()
	if len(res.Gaps) == 0 {
		t.Fatalf("no gaps recorded: %+v", res)
	}
	if res.Gaps[0].AfterSeq != 11 {
		t.Fatalf("gap after_seq=%d want 11", res.Gaps[0].AfterSeq)
	}
	if res.Gaps[0].MasterTS != 33000 {
		t.Fatalf("gap master_ts=%d want 33000", res.Gaps[0].MasterTS)
	}
}
