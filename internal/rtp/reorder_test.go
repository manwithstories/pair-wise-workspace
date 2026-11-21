package rtp

import (
	"testing"

	"github.com/pion/rtp"
)

func testConfig() Config {
	return Config{WindowSize: 8, BaseWaitMs: 100, RefBandwidthMbps: 3, MinWaitMs: 20, LatencyBudgetMs: 250}
}

func pkt(seq uint16) *rtp.Packet {
	return &rtp.Packet{Header: rtp.Header{Version: 2, SequenceNumber: seq}}
}

func arr(seq uint16, at, bw float64) Arrival {
	return Arrival{Packet: pkt(seq), ArrivalMs: at, BandwidthMbps: bw}
}

func seqs(pkts []*rtp.Packet) []uint16 {
	var out []uint16
	for _, p := range pkts {
		out = append(out, p.SequenceNumber)
	}
	return out
}

func TestDuplicatesCountedNotEmitted(t *testing.T) {
	r := NewReorderer(testConfig())
	out, st := r.Process([]Arrival{
		arr(1, 0, 3), arr(1, 5, 3), arr(2, 10, 3), arr(1, 15, 3),
	})
	if st.Duplicates != 2 {
		t.Fatalf("duplicates=%d want 2", st.Duplicates)
	}
	if len(out) != 2 {
		t.Fatalf("emitted=%d want 2: %v", len(out), seqs(out))
	}
}

func TestReorderWithinWindow(t *testing.T) {
	r := NewReorderer(testConfig())
	out, _ := r.Process([]Arrival{
		arr(1, 0, 3), arr(3, 10, 3), arr(2, 20, 3), arr(4, 30, 3),
	})
	got := seqs(out)
	want := []uint16{1, 2, 3, 4}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order=%v want %v", got, want)
		}
	}
}

func TestGapRecoveredWithinBoundedWait(t *testing.T) {
	r := NewReorderer(testConfig())
	out, st := r.Process([]Arrival{
		arr(1, 0, 3),
		arr(3, 50, 3),  // gap for 2 detected at t=50
		arr(2, 120, 3), // retransmission, 70ms after detection, limit 100
	})
	if st.Recovered != 1 || st.Lost != 0 {
		t.Fatalf("recovered=%d lost=%d, want 1/0", st.Recovered, st.Lost)
	}
	if got := seqs(out); len(got) != 3 || got[2] != 3 {
		t.Fatalf("order=%v want [1 2 3]", got)
	}
	if st.LossRecoveryRate() != 1 {
		t.Fatalf("rate=%v want 1", st.LossRecoveryRate())
	}
}

func TestGapDeclaredLostAtDeadline(t *testing.T) {
	r := NewReorderer(testConfig())
	// Gap for 2 detected at t=50, limit 100 → deadline 150.
	// Packet 3 (arrived t=50) is head-of-line blocked until the
	// deadline; its latency must not exceed the wait limit.
	_, st := r.Process([]Arrival{
		arr(1, 0, 3),
		arr(3, 50, 3),
		arr(4, 400, 3), // expiry evaluated at exact deadline 150
	})
	if st.Lost != 1 || st.Recovered != 0 {
		t.Fatalf("lost=%d recovered=%d, want 1/0", st.Lost, st.Recovered)
	}
	for i, lat := range st.Latencies {
		if lat > 100+1e-9 {
			t.Fatalf("latency[%d]=%v exceeds wait limit 100", i, lat)
		}
	}
	if st.LossRecoveryRate() != 0 {
		t.Fatalf("rate=%v want 0", st.LossRecoveryRate())
	}
}

func TestLatePacketIsolatedAndReclassified(t *testing.T) {
	r := NewReorderer(testConfig())
	out, st := r.Process([]Arrival{
		arr(1, 0, 3),
		arr(3, 50, 3),
		arr(4, 400, 3), // gap 2 expires at deadline 150 → lost
		arr(2, 500, 3), // arrives after close: isolated, reclassified
	})
	if st.Late != 1 {
		t.Fatalf("late=%d want 1", st.Late)
	}
	if st.Lost != 0 {
		t.Fatalf("lost=%d want 0 (reclassified to late)", st.Lost)
	}
	if got := seqs(out); len(got) != 3 {
		t.Fatalf("late packet must not be emitted: %v", got)
	}
}

func TestWaitLimitTracksBandwidthWithBudgetCap(t *testing.T) {
	cfg := testConfig()
	if got := cfg.waitLimit(3); got != 100 {
		t.Fatalf("waitLimit(3)=%v want 100", got)
	}
	if got := cfg.waitLimit(6); got != 50 {
		t.Fatalf("waitLimit(6)=%v want 50", got)
	}
	if got := cfg.waitLimit(0.5); got != 250 {
		t.Fatalf("waitLimit(0.5)=%v want budget cap 250", got)
	}
	if got := cfg.waitLimit(1000); got != 20 {
		t.Fatalf("waitLimit(1000)=%v want min 20", got)
	}
}

func TestWindowFullForcesExpiry(t *testing.T) {
	cfg := testConfig()
	cfg.WindowSize = 2
	r := NewReorderer(cfg)
	// Gap for 2 never fills; window holds at most 2 packets.
	_, st := r.Process([]Arrival{
		arr(1, 0, 3), arr(3, 10, 3), arr(4, 20, 3), arr(5, 30, 3),
	})
	if st.Lost != 1 {
		t.Fatalf("lost=%d want 1 (window forced expiry)", st.Lost)
	}
}

func TestLatencyP95(t *testing.T) {
	st := Stats{Latencies: []float64{10, 20, 30, 40, 50}}
	if got := st.LatencyP95(); got != 50 {
		t.Fatalf("p95=%v want 50", got)
	}
	if got := (Stats{}).LatencyP95(); got != 0 {
		t.Fatalf("empty p95=%v want 0", got)
	}
}
