package metrics

import (
	"testing"

	"mediaroute/internal/audio"
	"mediaroute/internal/clock"
	"mediaroute/internal/rtp"
	"mediaroute/internal/stream"
)

func TestPercentileNearestRank(t *testing.T) {
	xs := []float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	if got := percentile(xs, 95); got != 10 {
		t.Fatalf("p95 want 10 got %f", got)
	}
	if got := percentile(xs, 50); got != 5 {
		t.Fatalf("p50 want 5 got %f", got)
	}
	if got := percentile(nil, 95); got != 0 {
		t.Fatalf("empty percentile want 0 got %f", got)
	}
}

func TestSyncDeviationPicksNearest(t *testing.T) {
	ref := []float64{0, 40, 80, 120}
	cases := map[float64]float64{
		2:   2,
		38:  2,
		60:  20,
		200: 80,
	}
	for in, want := range cases {
		if got := SyncDeviation(in, ref); got != want {
			t.Fatalf("dev(%f) want %f got %f", in, want, got)
		}
	}
}

func TestCollectorAggregates(t *testing.T) {
	c := New()
	for i := 0; i < 10; i++ {
		c.Event(stream.Event{Kind: stream.EventSample, Latency: 2e6}) // 2 ms
		c.ObserveSync(1, int64(i), clock.Rational{Num: int64(i), Den: 1}, float64(i))
	}
	c.Event(stream.Event{Kind: stream.EventAudio, Audio: &audio.LoudnessResult{
		Samples: make([]float32, 9600), OutputLUFS: -16, TruePeakDBTP: -1.5, GainDB: 3,
	}})
	res := &stream.Result{Processed: 10, RTPStats: rtp.Stats{Received: 10, Delivered: 9, Recovered: 8, Lost: 1, ReorderDropped: 2}}
	r := c.Finalize(res, nil, false, false, nil)
	if r.ProcessedSamples != 10 {
		t.Fatalf("processed %d", r.ProcessedSamples)
	}
	if r.LatencyMSP95 < 1.9 || r.LatencyMSP95 > 2.1 {
		t.Fatalf("latency p95 %f", r.LatencyMSP95)
	}
	if r.SyncErrorMSMax != 9 {
		t.Fatalf("sync max %f", r.SyncErrorMSMax)
	}
	if r.PacketRecoveryRate != 0.8 {
		t.Fatalf("recovery rate want 0.8 got %f", r.PacketRecoveryRate)
	}
	if r.AudioFrames != 4800 {
		t.Fatalf("audio frames %d", r.AudioFrames)
	}
}

func TestSyncAbnormalFlaggedNotShifted(t *testing.T) {
	c := New()
	c.ObserveSyncDetail(SyncSample{TrackID: 1, Sample: 5, Deviation: 25})
	c.ObserveSyncDetail(SyncSample{TrackID: 1, Sample: 6, Deviation: 5})
	r := c.Finalize(nil, nil, false, false, nil)
	if r.SyncAbnormalCount != 1 {
		t.Fatalf("want 1 abnormal got %d", r.SyncAbnormalCount)
	}
	for _, s := range r.SyncSamples {
		if s.Sample == 5 && !s.Abnormal {
			t.Fatal("25 ms deviation must be abnormal")
		}
		if s.Sample == 6 && s.Abnormal {
			t.Fatal("5 ms deviation must not be abnormal")
		}
	}
}

func TestCheckThresholds(t *testing.T) {
	ok := Report{SyncErrorMSP95: 12, PacketRecoveryRate: 1}
	if err := ok.Check(); err != nil {
		t.Fatalf("healthy report rejected: %v", err)
	}
	bad := Report{SyncErrorMSP95: 25}
	if err := bad.Check(); err == nil {
		t.Fatal("sync threshold must fail")
	}
	slow := Report{PacketsReceived: 10, PacketsRecovered: 1, PacketsLost: 9, PacketRecoveryRate: 0.1}
	if err := slow.Check(); err == nil {
		t.Fatal("recovery threshold must fail")
	}
}
