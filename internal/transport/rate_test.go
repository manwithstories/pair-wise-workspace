package transport

import "testing"

func TestTransferMillisFollowsBandwidthSteps(t *testing.T) {
	trace := BandwidthTrace{{AtMS: 0, Kbps: 8}, {AtMS: 100, Kbps: 8000}}
	// 8000 bits = 1000 bytes. At 8 bits/ms for 100 ms sends 800 bits;
	// remaining 7200 bits at 8000 bits/ms takes 1 ms.
	got := TransferMillis(1000, 0, trace.Normalize())
	if got != 101 {
		t.Fatalf("TransferMillis=%d want 101", got)
	}
}

func TestPlanRecoveryWithinBudget(t *testing.T) {
	items := []RecoveryItem{{ID: PacketID{Rendition: "high", Sequence: 2}, Bytes: 1000}}
	trace := BandwidthTrace{{AtMS: 0, Kbps: 8000}}
	report := PlanRecovery(items, trace, RecoveryBudget{TotalMillis: 5, RTTMillis: 1, MaxAttempts: 2})
	if len(report.Recovered) != 1 || len(report.Failed) != 0 {
		t.Fatalf("report=%+v", report)
	}
	if report.ElapsedMS != 2 { // RTT 1 + 8000 bits / 8000 bits/ms
		t.Fatalf("elapsed=%d want 2", report.ElapsedMS)
	}
}

func TestPlanRecoveryBudgetExhaustionAndUnavailable(t *testing.T) {
	items := []RecoveryItem{
		{ID: PacketID{Rendition: "high", Sequence: 2}, Bytes: 1 << 20},
		{ID: PacketID{Rendition: "high", Sequence: 3}, Unavailable: true},
	}
	trace := BandwidthTrace{{AtMS: 0, Kbps: 64}}
	report := PlanRecovery(items, trace, RecoveryBudget{TotalMillis: 50, RTTMillis: 2})
	if len(report.Recovered) != 0 {
		t.Fatalf("unexpected recoveries: %+v", report.Recovered)
	}
	if len(report.Failed) != 2 {
		t.Fatalf("failed=%d want 2: %+v", len(report.Failed), report.Failed)
	}
	if report.Failed[1].Reason != "all_sources_unavailable" {
		t.Fatalf("second failure reason=%q", report.Failed[1].Reason)
	}
	if f := report.Failed[0].Fault(); f == nil || !f.Recoverable {
		t.Fatalf("fault=%v", f)
	}
}
