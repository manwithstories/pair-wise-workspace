package rtp_test

import (
	"testing"

	mrtp "medialab/internal/rtp"
)

func TestBandwidthTargetIsAdaptiveAndCapped(t *testing.T) {
	cases := map[int]int{
		1:    120,
		280:  97,
		900:  45,
		1200: 20,
		9999: 20,
	}
	for kbps, want := range cases {
		if got := mrtp.TargetForBandwidth(kbps); got != want || got > mrtp.MaxBufferTargetMS {
			t.Fatalf("TargetForBandwidth(%d)=%d, want %d and <=120", kbps, got, want)
		}
	}
}

func TestSweepDropsOnlyExpiredNonKeyFrames(t *testing.T) {
	b := mrtp.NewAdaptiveBuffer(900)
	_ = b.Enqueue(mrtp.TimedFrame{Sequence: 1, DeadlineMS: 10, Keyframe: false})
	_ = b.Enqueue(mrtp.TimedFrame{Sequence: 2, DeadlineMS: 10, Keyframe: true})
	dropped := b.Sweep(20)
	if len(dropped) != 1 || dropped[0].Sequence != 1 {
		t.Fatalf("dropped=%+v", dropped)
	}
	if len(b.Frames) != 1 || b.Frames[0].Sequence != 2 || b.DroppedExpiredNonKey != 1 {
		t.Fatalf("buffer=%+v", b)
	}
}

func TestAdaptiveQueueIsBounded(t *testing.T) {
	b := mrtp.NewAdaptiveBuffer(900)
	for i := 0; i < mrtp.MaxAdaptiveQueue; i++ {
		if err := b.Enqueue(mrtp.TimedFrame{Sequence: uint16(i)}); err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
	}
	if err := b.Enqueue(mrtp.TimedFrame{Sequence: 999}); err == nil {
		t.Fatal("expected bounded queue rejection")
	}
}
