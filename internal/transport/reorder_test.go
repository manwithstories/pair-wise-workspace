package transport

import (
	"errors"
	"testing"

	"mediapipe/internal/media"
)

func packets(n int) []media.Packet {
	ps := make([]media.Packet, n)
	for i := range ps {
		ps[i] = media.Packet{Sequence: uint64(i + 1), StreamIndex: i % 2, PTS: int64(i), DTS: int64(i)}
	}
	return ps
}

func TestSimulatorIsDeterministicAndImpaired(t *testing.T) {
	cfg := DefaultConfig(17, 48)
	a := NewSimulator(cfg, 48).Transmit(packets(48))
	b := NewSimulator(cfg, 48).Transmit(packets(48))
	if len(a) != len(b) {
		t.Fatalf("non deterministic lengths %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i].Sequence != b[i].Sequence {
			t.Fatalf("arrival order differs at %d", i)
		}
	}
	s := NewSimulator(cfg, 48)
	arrivals := s.Transmit(packets(48))
	if s.Stats.DuplicatePackets == 0 || s.Stats.ReorderedPackets == 0 {
		t.Fatalf("expected duplicates and reordering: %+v", s.Stats)
	}
	unique := map[uint64]bool{}
	for _, p := range arrivals {
		unique[p.Sequence] = true
	}
	if len(unique) >= 48 {
		t.Fatalf("expected losses to reduce unique arrivals: %d", len(unique))
	}
}

func TestReorderDedupAndCheckpointRecovery(t *testing.T) {
	rb := NewReorderBuffer(1)
	var ordered []media.Packet
	// Arrival: 1, duplicate 1, 3 buffered, 2 recovers contiguous 2-3.
	ordered = append(ordered, rb.Accept(media.Packet{Sequence: 1})...)
	ordered = append(ordered, rb.Accept(media.Packet{Sequence: 1})...)
	if got := rb.Accept(media.Packet{Sequence: 3}); got != nil {
		t.Fatal("seq 3 should buffer behind gap 2")
	}
	ordered = append(ordered, rb.Accept(media.Packet{Sequence: 2, Recovered: true})...)
	if len(ordered) != 3 || ordered[2].Sequence != 3 {
		t.Fatalf("ordering failed: %+v", ordered)
	}
	if rb.Stats.DuplicatePackets != 1 || rb.Stats.ReorderedPackets != 1 || rb.Stats.RecoveredPackets != 1 {
		t.Fatalf("bad stats: %+v", rb.Stats)
	}
	if rb.Checkpoint() != 3 {
		t.Fatalf("checkpoint = %d", rb.Checkpoint())
	}

	src := map[uint64]media.Packet{4: {Sequence: 4}, 5: {Sequence: 5}, 6: {Sequence: 6}}
	got, err := rb.Recover(6, 3, func(seq uint64) (media.Packet, bool, error) {
		p, ok := src[seq]
		return p, ok, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].Sequence != 4 || !got[0].Recovered {
		t.Fatalf("unexpected recovery: %+v", got)
	}
}

func TestSimulatorCreatesContiguousLossBursts(t *testing.T) {
	cfg := Config{Seed: 17, RecoverableLossCount: 8, BandwidthBaseKbps: 8000}
	s := NewSimulator(cfg, 64)
	missing := s.RecoverableMissing()
	adjacent := 0
	for i := 1; i < len(missing); i++ {
		if missing[i] == missing[i-1]+1 {
			adjacent++
		}
	}
	if adjacent == 0 {
		t.Fatalf("expected at least one contiguous loss burst: %v", missing)
	}
	if len(s.Bandwidth) != 64 || s.Stats.AvgBandwidthKbps <= 0 {
		t.Fatal("variable bandwidth trace was not generated")
	}
}

func TestUnrecoverableLossHasExitCodeThree(t *testing.T) {
	rb := NewReorderBuffer(1)
	_, err := rb.Recover(3, 0, func(seq uint64) (media.Packet, bool, error) {
		return media.Packet{}, false, nil
	})
	var me *media.Error
	if !errors.As(err, &me) || me.Code != media.CodeUnrecoverableLoss || me.ExitCode() != 3 {
		t.Fatalf("expected exit-3 loss error, got %v", err)
	}
}
