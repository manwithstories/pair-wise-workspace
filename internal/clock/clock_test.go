package clock

import (
	"math"
	"testing"
)

func TestUnwrapperHandles32BitRollover(t *testing.T) {
	u := NewUnwrapper()
	start := uint32(4294967000)
	ext0, _ := u.Observe(start)
	ext1, event := u.Observe(start + 4000) // wraps past 2^32
	if event != nil {
		t.Fatalf("ordinary rollover should not be a restart: %v", event)
	}
	if ext0 != uint64(start) {
		t.Fatalf("ext0=%d want %d", ext0, start)
	}
	if ext1 != 4294971000 {
		t.Fatalf("ext1=%d want 4294971000", ext1)
	}
	if u.Cycles() != 1 {
		t.Fatalf("cycles=%d want 1", u.Cycles())
	}
	if ext1 <= ext0 {
		t.Fatal("extended timestamp did not advance")
	}
}

func TestUnwrapperDistinguishesRestartAndNeverGoesBackwards(t *testing.T) {
	u := NewUnwrapper()
	seq := []uint32{4294967000, 3704, 5000, 7}
	var last uint64
	restartEvents := 0
	for i, ts := range seq {
		ext, event := u.Observe(ts)
		if i > 0 && ext <= last {
			t.Fatalf("extended time went backwards at %d: %d <= %d", i, ext, last)
		}
		if event != nil && event.Code == "clock_restart" {
			restartEvents++
		}
		last = ext
	}
	if restartEvents != 1 || u.Restarts() != 1 {
		t.Fatalf("restarts events=%d count=%d want 1", restartEvents, u.Restarts())
	}
}

func TestEstimateDriftMapsToNanoseconds(t *testing.T) {
	const rate = 90000
	const ppm = 180.0
	var samples []Sample
	for i := 0; i <= 1200; i++ {
		trueNS := int64(i) * 33_333_333
		ticks := int64(float64(trueNS) * float64(rate) / 1e9 * (1 + ppm/1e6))
		samples = append(samples, Sample{ExtendedTS: 4294967000 + uint64(ticks), ReferenceNS: trueNS})
	}
	model, merr := EstimateDrift(rate, samples)
	if merr != nil {
		t.Fatal(merr)
	}
	if math.Abs(model.DriftPPM-ppm) > 5 {
		t.Fatalf("drift=%.3f ppm want ~%.1f", model.DriftPPM, ppm)
	}
	mapped := model.Map(samples[len(samples)-1].ExtendedTS)
	want := samples[len(samples)-1].ReferenceNS
	if d := mapped - want; d > 100_000 || d < -100_000 {
		t.Fatalf("endpoint mapping error=%dns", d)
	}
}
