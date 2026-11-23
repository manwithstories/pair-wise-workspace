package rtp

import (
	"testing"

	"medpipe/internal/model"
)

func frames(n int) []*model.Frame {
	out := make([]*model.Frame, n)
	for i := range out {
		out[i] = &model.Frame{
			Seq: uint32(i), TimestampNS: int64(i) * 40_000_000,
			Keyframe: i%30 == 0, Payload: []byte{byte(i), byte(i >> 8), 0, 0},
		}
	}
	return out
}

func runSession(t *testing.T, cfg Config, n int) ([]*model.Frame, *Stats) {
	t.Helper()
	s := NewSession(cfg)
	for _, f := range frames(n) {
		if err := s.Send(f); err != nil {
			t.Fatal(err)
		}
	}
	got, stats, err := s.Receive()
	if err != nil {
		t.Fatal(err)
	}
	return got, stats
}

func TestNoLossFullRecovery(t *testing.T) {
	got, stats := runSession(t, Config{Seed: 1, LossBurst: 0, ReorderWindow: 2, BufferSlots: 64}, 60)
	if stats.FramesRecovered != 60 || stats.FramesLost != 0 || stats.RecoveryPct != 100 {
		t.Fatalf("stats wrong: %+v", stats)
	}
	if len(got) != 60 {
		t.Fatalf("recovered frames=%d", len(got))
	}
	// Time order must be preserved after jitter buffering.
	for i := 1; i < len(got); i++ {
		if got[i].TimestampNS <= got[i-1].TimestampNS {
			t.Fatal("recovered frames out of time order")
		}
	}
}

func TestBurstLossRecoversAboveThreshold(t *testing.T) {
	cfg := Config{Seed: 42, LossBurst: 3, BurstEvery: 40, ReorderWindow: 3, BufferSlots: 64}
	_, stats := runSession(t, cfg, 240)
	if stats.PacketsLost == 0 {
		t.Fatal("burst model produced no loss")
	}
	if stats.RecoveryPct < 95 {
		t.Fatalf("recovery %.2f%% below 95%%; lost=%d unrecoverable=%v",
			stats.RecoveryPct, stats.FramesLost, stats.UnrecoverableSeqs)
	}
	if stats.FramesRecovered+stats.FramesLost != stats.FramesSent {
		t.Fatal("frame accounting must balance")
	}
}

func TestDeterministicLossPattern(t *testing.T) {
	cfg := Config{Seed: 7, LossBurst: 2, BurstEvery: 25, ReorderWindow: 3, BufferSlots: 64}
	_, s1 := runSession(t, cfg, 120)
	_, s2 := runSession(t, cfg, 120)
	if s1.RecoveryPct != s2.RecoveryPct || s1.PacketsLost != s2.PacketsLost ||
		len(s1.UnrecoverableSeqs) != len(s2.UnrecoverableSeqs) {
		t.Fatal("same seed must yield identical outcomes")
	}
}

func TestUnrecoverableGapIsCountedNotFaked(t *testing.T) {
	// Force every transmission of keyframe 30 to fail: the gap crosses the
	// keyframe, so frames 30..59 cannot be decoded and must be reported as
	// unrecoverable losses rather than faked as successful.
	s := NewSession(Config{Seed: 42, LossBurst: 0, ReorderWindow: 0, BufferSlots: 64})
	in := frames(90)
	for _, f := range in {
		_ = s.Send(f)
	}
	for _, p := range s.sent {
		if p.frameSeq == 30 && !p.dropped {
			p.dropped = true
			s.lostCount++
		}
	}
	got, stats, err := s.Receive()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, seq := range stats.UnrecoverableSeqs {
		if seq == 30 {
			found = true
		}
	}
	if !found {
		t.Fatalf("keyframe-crossing gap must be listed: %+v", stats.UnrecoverableSeqs)
	}
	if stats.FramesLost == 0 || stats.RecoveryPct >= 100 {
		t.Fatal("loss must reduce recovery percentage instead of faking success")
	}
	// Frames after the next keyframe (60) must still be recovered in order.
	for i := 1; i < len(got); i++ {
		if got[i].TimestampNS <= got[i-1].TimestampNS {
			t.Fatal("recovered frames out of time order")
		}
	}
}

func TestKeyframeRedundancySurvivesSingleBurst(t *testing.T) {
	s := NewSession(Config{Seed: 42, LossBurst: 0, ReorderWindow: 0, BufferSlots: 64})
	kf := &model.Frame{Seq: 0, TimestampNS: 0, Keyframe: true, Payload: []byte{1, 2}}
	_ = s.Send(kf)
	// Drop the primary only; the redundant copy must still recover the frame.
	for _, p := range s.sent {
		if p.attempt == 0 {
			p.dropped = true
			s.lostCount++
		}
	}
	got, stats, err := s.Receive()
	if err != nil || len(got) != 1 || !got[0].Keyframe {
		t.Fatalf("keyframe redundancy failed: %+v err=%v", stats, err)
	}
}

func TestLatencyBounded(t *testing.T) {
	_, stats := runSession(t, Config{Seed: 5, LossBurst: 2, BurstEvery: 30, ReorderWindow: 2, BufferSlots: 64}, 120)
	for _, d := range stats.LatencyNS {
		if d < 18_000_000 || d >= 30_000_000 {
			t.Fatalf("latency sample out of bounded range: %d", d)
		}
	}
}
