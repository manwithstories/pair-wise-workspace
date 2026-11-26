package jitter

import (
	"testing"

	rtpio "mediaqc/internal/rtp"
)

func pkt(seq uint64, arrival int64) *rtpio.Packet {
	return &rtpio.Packet{SSRC: 7, ExtSequence: seq, Sequence: uint16(seq), ArrivalMS: arrival}
}

func TestBufferReordersWithinWindow(t *testing.T) {
	b := NewBuffer(7, 100, 40, 16)
	if err := b.Push(pkt(101, 5)); err != nil {
		t.Fatal(err)
	}
	if out, gaps := b.Advance(20); len(out) != 0 || len(gaps) != 0 {
		t.Fatalf("packet released too early: out=%d gaps=%d", len(out), len(gaps))
	}
	if err := b.Push(pkt(100, 30)); err != nil {
		t.Fatal(err)
	}
	out, gaps := b.Advance(30)
	if len(gaps) != 0 || len(out) != 2 {
		t.Fatalf("expected reordered release, out=%d gaps=%+v", len(out), gaps)
	}
	if out[0].ExtSequence != 100 || out[1].ExtSequence != 101 {
		t.Fatalf("order=%d,%d", out[0].ExtSequence, out[1].ExtSequence)
	}
	if b.Stats.Reordered != 1 {
		t.Fatalf("stats=%+v", b.Stats)
	}
}

func TestBufferCreatesGapOutsideWindow(t *testing.T) {
	b := NewBuffer(7, 200, 20, 16)
	if err := b.Push(pkt(201, 0)); err != nil {
		t.Fatal(err)
	}
	out, gaps := b.Advance(25)
	if len(out) != 1 || out[0].ExtSequence != 201 {
		t.Fatalf("expected future packet after gap, out=%+v", out)
	}
	if len(gaps) != 1 || gaps[0].FromExtSeq != 200 || gaps[0].ToExtSeq != 200 {
		t.Fatalf("gaps=%+v", gaps)
	}
	if err := b.Push(pkt(200, 30)); err != nil {
		t.Fatal(err)
	}
	if b.Stats.Late != 1 {
		t.Fatalf("late packet stats=%+v", b.Stats)
	}
}

func TestBufferBurstGapRangeAndTailFlush(t *testing.T) {
	b := NewBuffer(7, 300, 10, 32)
	if err := b.Push(pkt(305, 10)); err != nil {
		t.Fatal(err)
	}
	firstOut, gaps := b.Advance(30)
	if len(firstOut) != 1 || firstOut[0].ExtSequence != 305 {
		t.Fatalf("future packet should be released with gap: %+v", firstOut)
	}
	if len(gaps) != 1 || gaps[0].Len() != 5 || gaps[0].FromExtSeq != 300 || gaps[0].ToExtSeq != 304 {
		t.Fatalf("burst gap=%+v", gaps)
	}
	if gaps[0].Missing[0] != 300 || gaps[0].Missing[4] != 304 {
		t.Fatalf("missing=%v", gaps[0].Missing)
	}
	out, tail := b.FlushThrough(307)
	if len(out) != 0 || len(tail) != 1 || tail[0].Len() != 2 {
		t.Fatalf("out=%d tail=%+v", len(out), tail)
	}
	if tail[0].FromExtSeq != 306 || tail[0].ToExtSeq != 307 {
		t.Fatalf("tail gap=%+v", tail)
	}
}

func TestBufferBoundedQueue(t *testing.T) {
	b := NewBuffer(7, 1, 1000, 1)
	if err := b.Push(pkt(2, 0)); err != nil {
		t.Fatal(err)
	}
	if err := b.Push(pkt(3, 0)); err == nil {
		t.Fatal("expected bounded queue full error")
	}
}

func TestBufferAdaptiveWindowChange(t *testing.T) {
	b := NewBuffer(7, 400, 30, 8)
	b.SetWaitMS(80)
	_ = b.Push(pkt(401, 0))
	if _, gaps := b.Advance(40); len(gaps) != 0 {
		t.Fatal("window should have expanded")
	}
	if _, gaps := b.Advance(81); len(gaps) != 1 {
		t.Fatal("expired adaptive window should form gap")
	}
}
