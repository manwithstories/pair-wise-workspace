package rtp

import (
	"strings"
	"testing"
)

func TestJitterBufferExpiresOldPackets(t *testing.T) {
	jb, err := NewJitterBuffer(JitterOptions{TargetDelayNanos: 50, MaxPackets: 4})
	if err != nil {
		t.Fatal(err)
	}
	jb.Insert(Packet{Frame: 0, Keyframe: true}, 0)
	jb.Insert(Packet{Frame: 1}, 10)
	jb.Insert(Packet{Frame: 3}, 20)
	// Advance the clock so the first three pass their deadline; the insert
	// itself performs the deadline playout and reports the frame gap.
	out, errs := jb.Insert(Packet{Frame: 4}, 200)
	for _, p := range jb.Flush() {
		out = append(out, p)
	}
	if len(out) == 0 {
		t.Fatal("no packets released")
	}
	// Frame 2 never arrived: gap -> keyframe request.
	sawKeyframeReq := false
	for _, e := range errs {
		if e != nil && strings.Contains(e.Error(), "requesting keyframe") {
			sawKeyframeReq = true
		}
	}
	if !sawKeyframeReq || !jb.WaitingForKeyframe() {
		t.Fatalf("missing keyframe request: %v", errs)
	}
	// A keyframe clears the wait.
	jb.Insert(Packet{Frame: 10, Keyframe: true}, 210)
	jb.Flush()
	if jb.WaitingForKeyframe() {
		t.Fatal("keyframe did not clear the wait")
	}
}

func TestJitterBufferOverflowIsBounded(t *testing.T) {
	jb, _ := NewJitterBuffer(JitterOptions{TargetDelayNanos: 1 << 40, MaxPackets: 3})
	var released int
	for i := 0; i < 10; i++ {
		out, _ := jb.Insert(Packet{Frame: i, Seq: uint16(i)}, int64(i))
		released += len(out)
		if jb.Depth() > 3 {
			t.Fatalf("depth=%d exceeds cap", jb.Depth())
		}
	}
	if released == 0 {
		t.Fatal("overflow released nothing")
	}
	if jb.Stats().MaxDepth > 3 {
		t.Fatalf("max depth=%d", jb.Stats().MaxDepth)
	}
}

func TestJitterRejectsLateFrame(t *testing.T) {
	jb, _ := NewJitterBuffer(JitterOptions{TargetDelayNanos: 0, MaxPackets: 8})
	jb.Insert(Packet{Frame: 0, Keyframe: true}, 0)
	jb.Playout()
	jb.Insert(Packet{Frame: 5, Keyframe: false}, 10)
	jb.Playout()
	// Frame 3 arrives after 5 already played: must be dropped.
	_, errs := jb.Insert(Packet{Frame: 3}, 20)
	if len(errs) == 0 {
		t.Fatal("late frame not reported")
	}
	if jb.Stats().Dropped == 0 {
		t.Fatal("late frame not counted")
	}
}

func TestFlushPreservesTailReleasedByDeadline(t *testing.T) {
	// Regression: the final packet is often released by the deadline playout
	// that Flush triggers; Flush must include it or the stream tail vanishes.
	jb, _ := NewJitterBuffer(JitterOptions{TargetDelayNanos: 10, MaxPackets: 32})
	jb.Insert(Packet{Frame: 0, Keyframe: true}, 0)
	jb.Insert(Packet{Frame: 1}, 100)
	out := jb.Flush()
	if len(out) != 1 || out[0].Frame != 1 {
		t.Fatalf("flush lost the tail: %+v", out)
	}
}
