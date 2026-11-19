package rtprepair_test

import (
	"testing"
	"time"

	"mediasync/internal/rtprepair"
	"mediasync/internal/testdata"
)

func packets(n, size int) [][]byte {
	out := make([][]byte, n)
	for i := range out {
		out[i] = make([]byte, size)
	}
	return out
}

func TestBandwidthProfileCycles(t *testing.T) {
	p := rtprepair.DefaultBandwidthProfile([]int{800, 2400, 600})
	cases := map[time.Duration]int{
		0:                      800,
		250 * time.Millisecond: 2400,
		500 * time.Millisecond: 600,
		750 * time.Millisecond: 800,
	}
	for at, want := range cases {
		if got := p.RateAt(at); got != want {
			t.Errorf("RateAt(%v) = %d, want %d", at, got, want)
		}
	}
}

func TestSlowLinkDelaysWithoutLosing(t *testing.T) {
	fx := testdata.GenerateRTP(testdata.RTPPlan{Packets: 60, PayloadSize: 1200, SSRC: 1})
	slow := rtprepair.BandwidthProfile{Kbps: []int{400}, Interval: 500 * time.Millisecond,
		MaxDelay: 10 * time.Second}
	res := rtprepair.Shape(fx.Packets, slow)
	if res.DroppedCount != 0 {
		t.Errorf("slow but adequate link dropped %d packets", res.DroppedCount)
	}
	// Arrivals never go backwards.
	for i := 1; i < len(res.Arrivals); i++ {
		if res.Arrivals[i] < res.Arrivals[i-1] {
			t.Fatalf("arrival %d (%v) precedes %d (%v)", i, res.Arrivals[i], i-1, res.Arrivals[i-1])
		}
	}
	// 60 packets of 1200 bytes at 400kbps exceed the one-second burst bucket,
	// so the tail must wait for tokens instead of being dropped.
	if res.Elapsed < 300*time.Millisecond {
		t.Errorf("elapsed = %v, expected the slow link to spread the burst", res.Elapsed)
	}
	if res.MaxDelay <= 0 {
		t.Error("max delay not recorded")
	}
}

func TestBandwidthCollapseDropsObservablePackets(t *testing.T) {
	fx := testdata.GenerateRTP(testdata.RTPPlan{Packets: 400, PayloadSize: 1400, SSRC: 1})
	profile := rtprepair.BandwidthProfile{
		Kbps:     []int{2400, 64, 2400},
		Interval: 100 * time.Millisecond,
		MaxDelay: 50 * time.Millisecond,
	}
	res := rtprepair.Shape(fx.Packets, profile)
	if res.DroppedCount == 0 {
		t.Fatal("a 64kbps trough did not cause any shaper drops")
	}
	if len(res.Samples) == 0 {
		t.Fatal("no bandwidth samples recorded")
	}
	seen := false
	for _, s := range res.Samples {
		if s.LinkKbps == 64 && s.Dropped > 0 {
			seen = true
		}
	}
	if !seen {
		t.Error("no sample attributes drops to the collapsed window")
	}
	if res.MinKbps != 64 || res.MaxKbps != 2400 {
		t.Errorf("rate range = %d..%d", res.MinKbps, res.MaxKbps)
	}
}

func TestShapeIsDeterministic(t *testing.T) {
	fx := testdata.GenerateRTP(testdata.RTPPlan{Packets: 100, PayloadSize: 400, SSRC: 1})
	p := rtprepair.DefaultBandwidthProfile([]int{800, 2400, 600})
	a := rtprepair.Shape(fx.Packets, p)
	b := rtprepair.Shape(fx.Packets, p)
	if a.DroppedCount != b.DroppedCount || len(a.Samples) != len(b.Samples) {
		t.Fatal("shaping is not deterministic")
	}
	for i := range a.Arrivals {
		if a.Arrivals[i] != b.Arrivals[i] {
			t.Fatalf("arrival %d differs between runs", i)
		}
	}
}

func TestShapeEmptyProfileDefaults(t *testing.T) {
	fx := testdata.GenerateRTP(testdata.RTPPlan{Packets: 5, PayloadSize: 100, SSRC: 1})
	res := rtprepair.Shape(fx.Packets, rtprepair.BandwidthProfile{})
	if res.MinKbps != 800 {
		t.Errorf("default rate = %d, want 800", res.MinKbps)
	}
	if res.Summary() == "" {
		t.Error("empty summary")
	}
}
