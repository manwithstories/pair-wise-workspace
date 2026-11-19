package repair

import (
	"testing"

	"github.com/pion/rtcp"
)

func TestTimestampErrorModel(t *testing.T) {
	r := NewTSRepair(1000)
	r.RegisterTrack(1, 90000)

	// Wrap: located at the wrapping packet, zero deviation.
	r.Map(1, 100, 0xFFFFFFF0)
	_, _, err := r.Map(1, 101, 0x10)
	if err == nil || err.Kind != "wrap" || err.ExtSeq != 101 {
		t.Fatalf("wrap: %+v", err)
	}

	// Backward: located with deviation in nanoseconds.
	r2 := NewTSRepair(1000)
	r2.RegisterTrack(2, 90000)
	r2.Map(2, 0, 0)
	r2.Map(2, 1, 90000)
	_, _, err = r2.Map(2, 2, 45000)
	if err == nil || err.Kind != "backward" || err.ExtSeq != 2 || err.DeviationNs != 500_000_000 {
		t.Fatalf("backward: %+v", err)
	}
}

func TestDriftErrorLocated(t *testing.T) {
	r := NewTSRepair(500) // 500 ppm tolerance
	r.RegisterTrack(1, 90000)
	r.Map(1, 0, 0)
	if e := r.AddSenderReport(&rtcp.SenderReport{SSRC: 1, NTPTime: 10 << 32, RTPTime: 0}); e != nil {
		t.Fatalf("first SR: %+v", e)
	}
	r.Map(1, 90, 90000)
	if e := r.AddSenderReport(&rtcp.SenderReport{SSRC: 1, NTPTime: 11 << 32, RTPTime: 90000}); e != nil {
		t.Fatalf("consistent SR: %+v", e)
	}
	r.Map(1, 180, 180000)
	// NTP advanced 2s while RTP advanced only 1s worth of ticks.
	e := r.AddSenderReport(&rtcp.SenderReport{SSRC: 1, NTPTime: 13 << 32, RTPTime: 180000})
	if e == nil || e.Kind != "drift" || e.ExtSeq != 180 {
		t.Fatalf("drift: %+v", e)
	}
	if e.DeviationNs <= 0 {
		t.Fatalf("deviation must be positive: %+v", e)
	}
}

func TestEstimatedFlag(t *testing.T) {
	r := NewTSRepair(1000)
	r.RegisterTrack(1, 90000)
	r.Map(1, 0, 0)
	if !r.Estimated(1) {
		t.Fatal("should be estimated before SR")
	}
	r.AddSenderReport(&rtcp.SenderReport{SSRC: 1, NTPTime: 1 << 32, RTPTime: 0})
	if r.Estimated(1) {
		t.Fatal("should be anchored after SR")
	}
}

func TestClockStateRoundTrip(t *testing.T) {
	r := NewTSRepair(1000)
	r.RegisterTrack(1, 90000)
	ns1, _, _ := r.Map(1, 0, 1000)
	st := r.State()

	r2 := NewTSRepair(1000)
	r2.RegisterTrack(1, 90000)
	r2.Restore(st)
	ns2, _, _ := r2.Map(1, 1, 1000+3000)
	want := ns1 + 3000*1_000_000_000/90000
	if ns2 != want {
		t.Fatalf("ns=%d want %d", ns2, want)
	}
}
