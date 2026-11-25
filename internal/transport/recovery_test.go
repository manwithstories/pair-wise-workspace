package transport

import (
	"context"
	"testing"

	"avlab/internal/media"
)

func runEngine(t *testing.T) *EngineResult {
	t.Helper()
	tr := NewTrace(17, 4096)
	eng := NewEngine(context.Background(), EngineConfig{
		Packets: 4096, Capacity: 512,
		WaitTick: 20, PayloadLen: 24,
	}, tr)
	res, err := eng.Run()
	if err != nil {
		t.Fatalf("engine run: %v", err)
	}
	return res
}

func TestTraceDeterminism(t *testing.T) {
	tr1 := NewTrace(17, 4096)
	tr2 := NewTrace(17, 4096)
	if len(tr1.Steps) != len(tr2.Steps) {
		t.Fatal("trace step count differs across runs")
	}
	for i := range tr1.Steps {
		if tr1.Steps[i] != tr2.Steps[i] {
			t.Fatalf("step %d differs: %+v vs %+v", i, tr1.Steps[i], tr2.Steps[i])
		}
	}
}

func TestReorderDedupAndEpoch(t *testing.T) {
	rb := NewReorderBuffer()
	mk := func(seq uint16, ep media.Epoch) *Packet {
		return &Packet{Epoch: ep, RTP: rtpHeader(seq)}
	}
	if err := rb.Push(mk(10, 0)); err != nil {
		t.Fatal(err)
	}
	if err := rb.Push(mk(10, 0)); !media.IsCode(err, media.CodePacketDuplicate) {
		t.Fatalf("dup err = %v", err)
	}
	if err := rb.Push(mk(12, 0)); err != nil {
		t.Fatal(err)
	}
	if err := rb.Push(mk(11, 0)); err != nil {
		t.Fatal(err)
	}
	out := rb.Drain()
	if len(out) != 3 || out[0].Seq() != 10 || out[2].Seq() != 12 {
		t.Fatalf("drain order wrong: %v", out)
	}
	// stale epoch packet must be rejected
	if err := rb.Push(mk(13, 0)); err != nil {
		t.Fatal(err)
	}
	if err := rb.Push(mk(9, 0)); !media.IsCode(err, media.CodePacketDuplicate) {
		t.Fatalf("already-released seq err = %v", err)
	}
	// new epoch resets the timeline
	if err := rb.Push(mk(0, 1)); err != nil {
		t.Fatalf("new epoch: %v", err)
	}
	if err := rb.Push(mk(5, 0)); !media.IsCode(err, media.CodePacketLate) {
		t.Fatalf("late epoch err = %v", err)
	}
	if s := rb.Stats(); s.Duplicate != 2 || s.LateEpoch != 1 {
		t.Fatalf("stats = %+v", s)
	}
}

func TestJitterBufferBoundedAndRecovery(t *testing.T) {
	jb := NewJitterBuffer(4, 10)
	pk := func(seq uint16, kf bool) *Packet {
		return &Packet{Epoch: 0, RTP: rtpHeader(seq), Keyframe: kf}
	}
	for _, seq := range []uint16{0, 1} {
		if err := jb.Push(pk(seq, seq == 0)); err != nil {
			t.Fatal(err)
		}
	}
	if got := jb.Playout(1); len(got) != 2 {
		t.Fatalf("playout = %d packets", len(got))
	}
	// gap at seq 2 with deadline 10
	jb.MarkLost(2, 1, false)
	if got := jb.Playout(5); len(got) != 0 {
		t.Fatal("gap must block playout inside wait window")
	}
	// retransmission arrives in time
	if err := jb.Push(pk(2, false)); err != nil {
		t.Fatal(err)
	}
	if got := jb.Playout(6); len(got) != 1 || got[0].Seq() != 2 {
		t.Fatalf("recovered playout = %v", got)
	}
	// hard capacity
	for i := uint16(3); i < 10; i++ {
		_ = jb.Push(pk(i, i%32 == 0))
	}
	if err := jb.Push(pk(99, true)); !media.IsCode(err, media.CodeLimitExceeded) {
		t.Fatalf("capacity err = %v", err)
	}
}

func TestDeadlineSkipToKeyframe(t *testing.T) {
	jb := NewJitterBuffer(64, 5)
	pk := func(seq uint16, kf bool) *Packet {
		return &Packet{Epoch: 0, RTP: rtpHeader(seq), Keyframe: kf}
	}
	if err := jb.Push(pk(0, true)); err != nil {
		t.Fatal(err)
	}
	jb.Playout(1)
	jb.MarkLost(1, 1, false)
	// buffered non-keyframes followed by a keyframe at seq 4
	for _, seq := range []uint16{2, 3} {
		if err := jb.Push(pk(seq, false)); err != nil {
			t.Fatal(err)
		}
	}
	if err := jb.Push(pk(4, true)); err != nil {
		t.Fatal(err)
	}
	// before the deadline nothing past the gap may play
	if got := jb.Playout(5); len(got) != 0 {
		t.Fatalf("premature playout: %v", got)
	}
	// after expiry, skip non-keyframes and land on the keyframe
	got := jb.Playout(12)
	if len(got) != 1 || got[0].Seq() != 4 || !got[0].Keyframe {
		t.Fatalf("keyframe jump result = %v", got)
	}
	st := jb.Stats()
	if st.DeadlineSkips != 1 || st.KeyframeJumps != 1 {
		t.Fatalf("stats = %+v", st)
	}
}

func TestEngineCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	tr := NewTrace(17, 1_000_000_000)
	eng := NewEngine(ctx, EngineConfig{
		Packets: 1_000_000_000, Capacity: 64, WaitTick: 20,
	}, tr)
	cancel()
	if _, err := eng.Run(); !media.IsCode(err, media.CodeCanceled) {
		t.Fatalf("cancel err = %v, want CodeCanceled", err)
	}
}

func TestEngineScorecard(t *testing.T) {
	res := runEngine(t)
	t.Logf("reorder=%+v", res.Reorder)
	t.Logf("jitter=%+v", res.Jitter)
	t.Logf("trace dup=%d reo=%d drops=%d bwsteps=%d",
		res.Trace.Duplicates, res.Trace.Reordered,
		res.Trace.BurstDrops, len(res.Trace.BwSteps))
	if res.Trace.BurstDrops != 20 {
		t.Fatalf("burst drops = %d, want 20", res.Trace.BurstDrops)
	}
	if len(res.Trace.BwSteps) != 4 {
		t.Fatalf("bandwidth stairs = %d, want 4", len(res.Trace.BwSteps))
	}
	if res.Jitter.BurstRecovered != 16 || res.Jitter.BurstUnrecovered != 4 {
		t.Fatalf("recovery = %d/%d, want 16/20 recovered, 4 expired",
			res.Jitter.BurstRecovered, res.Trace.BurstDrops)
	}
	if res.Jitter.DeadlineSkips != 4 {
		t.Fatalf("deadline skips = %d, want 4", res.Jitter.DeadlineSkips)
	}
	if res.Trace.Duplicates != 24 {
		t.Fatalf("trace duplicates = %d, want 24", res.Trace.Duplicates)
	}
	if res.Reorder.Duplicate < 24 {
		t.Fatalf("reorder duplicates = %d, want >= 24", res.Reorder.Duplicate)
	}
	if res.Reorder.Reordered != 16 {
		t.Fatalf("reordered = %d, want 16", res.Reorder.Reordered)
	}
	if res.Jitter.QueuePeak == 0 || res.Jitter.QueuePeak > 512 {
		t.Fatalf("queue peak = %d outside (0,512]", res.Jitter.QueuePeak)
	}
}
