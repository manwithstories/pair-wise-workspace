package clock

import (
	"math"
	"testing"

	"archiver/internal/media"
)

const ntpEpoch = ntpUnixEpochDelta

func TestNominalMappingWithoutReports(t *testing.T) {
	m := NewModel(44100)
	us, bound := m.ToMediaTime(44100) // one second worth of ticks
	if us < 999_000 || us > 1_001_000 {
		t.Fatalf("us = %d, want ~1s", us)
	}
	if bound <= 0 {
		t.Fatalf("error bound must be positive without anchors, got %d", bound)
	}
}

func TestWrapExtension(t *testing.T) {
	m := NewModel(90000)
	if _, _ = m.ToMediaTime(0xfffffff0); m.lastExt != 0xfffffff0 {
		t.Fatalf("lastExt = %x", m.lastExt)
	}
	us, _ := m.ToMediaTime(0x10) // wrapped: ext = 0x1_0000_0010
	if m.lastExt != 0x1_0000_0010 {
		t.Fatalf("lastExt = %x, want wrap extension", m.lastExt)
	}
	want := media.TimeUs(math.Round(float64(0x1_0000_0010) * 1e6 / 90000))
	if us != want {
		t.Fatalf("us = %d, want %d", us, want)
	}
}

func TestDriftFitFromSenderReports(t *testing.T) {
	m := NewModel(90000)
	// Sender clock runs 50ppm fast: after 10s of wall time the RTP
	// clock advanced 900000*1.00005 ticks.
	m.AddSenderReport(ntpEpoch+1000, 0, 0)
	m.AddSenderReport(ntpEpoch+1010, 0, 900045)
	us, _ := m.ToMediaTime(1800090) // 20s of drifted sender time
	// Media time is absolute (Unix microseconds): 1000s base + 20s.
	if us < 1_019_990_000 || us > 1_020_010_000 {
		t.Fatalf("us = %d, want ~1020s absolute", us)
	}
	// Fitted slope must reflect the +50ppm drift.
	want := 1e6 / 90000 * 1.00005
	if math.Abs(m.slope-want) > want*1e-4 {
		t.Fatalf("slope = %v, want ~%v", m.slope, want)
	}
}

func TestMonotonicGuard(t *testing.T) {
	m := NewModel(90000)
	m.AddSenderReport(ntpEpoch+1000, 0, 0)
	us1, _ := m.ToMediaTime(90000)
	// A second anchor implying a backwards jump must not move the
	// emitted media time backwards.
	m.AddSenderReport(ntpEpoch+999, 0, 180000)
	us2, _ := m.ToMediaTime(180000)
	if us2 < us1 {
		t.Fatalf("media time moved backwards: %d -> %d", us1, us2)
	}
}

func TestSnapshotRestore(t *testing.T) {
	m := NewModel(44100)
	m.AddSenderReport(ntpEpoch+1000, 1<<31, 44100)
	if _, _ = m.ToMediaTime(88200); true {
	}
	r := RestoreModel(m.Snapshot())
	us1, _ := m.ToMediaTime(132300)
	us2, _ := r.ToMediaTime(132300)
	if us1 != us2 {
		t.Fatalf("restored model diverges: %d != %d", us1, us2)
	}
}
