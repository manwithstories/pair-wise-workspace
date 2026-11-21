package sync

import (
	"testing"

	"mediaroute/internal/model"
)

func pairs(n int, aStep, vStep uint32, a0, v0 uint32) []Pair {
	var ps []Pair
	for i := 0; i < n; i++ {
		ps = append(ps, Pair{Audio: a0 + uint32(i)*aStep, Video: v0 + uint32(i)*vStep})
	}
	return ps
}

func TestWithinThreshold(t *testing.T) {
	a := Aligner{ThresholdMs: 40, AudioRate: 48000, VideoRate: 90000,
		AudioBase: 0, AudioBaseMs: 0, VideoBase: 0, VideoBaseMs: 0}
	// video presentation runs 35 ms ahead (3150 units at 90 kHz)
	al := a.Align(pairs(8, 48000/25, 90000/25, 0, 3150))
	if al.MeasuredMs != -35 {
		t.Fatalf("skew=%d want -35", al.MeasuredMs)
	}
	if al.CorrectionMs != 0 || al.ResidualMs != -35 {
		t.Fatalf("within threshold must not correct: %+v", al)
	}
}

func TestCorrectionBeyondThreshold(t *testing.T) {
	a := Aligner{ThresholdMs: 40, AudioRate: 48000, VideoRate: 90000}
	al := a.Align(pairs(8, 1920, 3600, 0, 10800)) // video 120 ms ahead
	if al.MeasuredMs != -120 || al.CorrectionMs != 120 || al.ResidualMs != 0 {
		t.Fatalf("bad correction: %+v", al)
	}
}

func TestWrapNotAJump(t *testing.T) {
	// audio clock starts 2 samples before uint32 wrap
	a0 := uint32(0xFFFFFFFF) - 2*1920
	a := Aligner{ThresholdMs: 40, AudioRate: 48000, VideoRate: 90000,
		AudioBase: a0, AudioBaseMs: 0, VideoBase: 0, VideoBaseMs: 0}
	al := a.Align(pairs(8, 1920, 3600, a0, 0))
	if al.MeasuredMs != 0 {
		t.Fatalf("wrap misread as jump: skew=%d", al.MeasuredMs)
	}
}

func TestAlignErrorPropagation(t *testing.T) {
	inner := &model.MediaError{Code: model.ErrGap, Module: "recovery",
		Track: "v1", Seq: 18, Timestamp: 540000, Msg: "no redundant data"}
	err := AlignError(inner, "cannot align across gap")
	me, ok := err.(*model.MediaError)
	if !ok || me.Track != "v1" || me.Seq != 18 || me.Timestamp != 540000 {
		t.Fatalf("context lost: %+v", me)
	}
	if model.ExitCode(me) != model.ExitUnrecoverable {
		t.Fatalf("exit code changed")
	}
}
