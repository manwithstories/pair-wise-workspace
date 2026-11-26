package metrics

import (
	"strings"
	"testing"

	"mediad/internal/recover"
)

func TestComputeAggregatesRecoveryAndP95(t *testing.T) {
	r1 := &recover.Report{Packets: 10, LostPackets: 2, RecoveredPackets: 2, Retransmissions: 3,
		Stats: []recover.PacketStat{{LatencyMS: 10}, {LatencyMS: 100}, {LatencyMS: 200}}}
	r2 := &recover.Report{Packets: 5, LostPackets: 1, RecoveredPackets: 0, UnrecoveredPackets: 1,
		Stats: []recover.PacketStat{{LatencyMS: 50}}}
	m := Compute(5_000_000, []*recover.Report{r1, r2}, 0, 0, "abc")
	if m.SyncMaxMS != 5 {
		t.Fatalf("sync=%.1f", m.SyncMaxMS)
	}
	if m.LostPackets != 3 || m.UnrecoveredPackets != 1 {
		t.Fatalf("counts=%#v", m)
	}
	// 2/3 recovered.
	if m.LossRecoveryPct < 66.6 || m.LossRecoveryPct > 66.7 {
		t.Fatalf("recovery=%.2f", m.LossRecoveryPct)
	}
	if m.LatencyP95MS != 200 {
		t.Fatalf("p95=%.1f", m.LatencyP95MS)
	}
	if m.Pass() {
		t.Fatal("expected threshold failure due recovery and latency")
	}
	lines := strings.Join(m.KeyValueLines(), "\n")
	for _, key := range []string{"sync_max_ms=", "loss_recovery_pct=", "latency_p95_ms=", "index_seek_miss=0", "boundary_mismatch=0", "output_sha256=abc"} {
		if !strings.Contains(lines, key) {
			t.Fatalf("missing %q in\n%s", key, lines)
		}
	}
}

func TestPassingMetrics(t *testing.T) {
	m := Metrics{SyncMaxMS: 8.3, LossRecoveryPct: 100, LatencyP95MS: 121, IndexSeekMiss: 0, BoundaryMismatch: 0}
	if !m.Pass() {
		t.Fatalf("failures=%v", m.Failures())
	}
}
