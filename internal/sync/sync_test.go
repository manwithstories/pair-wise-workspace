package sync

import (
	"math/rand/v2"
	"testing"
	"time"

	"avlab/internal/media"
)

func TestClockMappingRoundTrip(t *testing.T) {
	m := NewClockMapping(0, 0)
	// one second: 48000 audio samples and 90000 video ticks
	tick, err := m.AudioToTick(48000)
	if err != nil || tick != media.MediaTime(90000) {
		t.Fatalf("AudioToTick(48000) = %d, %v", tick, err)
	}
	if got := m.TickToAudio(90000); got != 48000 {
		t.Fatalf("TickToAudio(90000) = %d, want 48000", got)
	}
	// anchored at a non-zero origin, still consistent
	m2 := NewClockMapping(media.FromDuration(5*time.Second), 240000)
	videoAt6s := media.FromDuration(6 * time.Second)
	audioAt6s := m2.TickToAudio(videoAt6s)
	back, err := m2.AudioToTick(audioAt6s)
	if err != nil || back != videoAt6s {
		t.Fatalf("anchored round trip = %d, %v; want %d", back, err, videoAt6s)
	}
}

// driftSeries builds the canonical bounded-jitter series used across tests.
func driftSeries(t *testing.T, seed uint64, pairs int) (*Aligner, *DriftReport) {
	t.Helper()
	rng := rand.New(rand.NewPCG(seed, seed^0x1234567))
	m := NewClockMapping(0, 0)
	a := NewAligner(m, 20*time.Millisecond)
	for i := 0; i < pairs; i++ {
		videoTick := media.MediaTime((i + 1) * 9000) // 100 ms steps
		expected := m.TickToAudio(videoTick)
		// bounded jitter: +/- 720 audio samples = +/- 15 ms
		jitter := int64(rng.Uint64N(1441)) - 720
		if err := a.Observe(videoTick, uint64(int64(expected)+jitter)); err != nil {
			t.Fatalf("observe %d: %v", i, err)
		}
	}
	return a, a.Report()
}

func TestDriftQuantilesWithinThreshold(t *testing.T) {
	_, r := driftSeries(t, 17, 256)
	if r.Pairs != 256 {
		t.Fatalf("pairs = %d", r.Pairs)
	}
	if r.P95MS > 20 {
		t.Fatalf("p95 drift = %.3f ms, want <= 20", r.P95MS)
	}
	if r.MaxAbsMS > 15.0001 {
		t.Fatalf("max drift = %.3f ms exceeds bounded jitter 15", r.MaxAbsMS)
	}
	if r.Failed {
		t.Fatalf("bounded jitter marked failure: %+v", r)
	}
}

func TestDriftDeterminism(t *testing.T) {
	_, r1 := driftSeries(t, 17, 128)
	_, r2 := driftSeries(t, 17, 128)
	if r1.P95MS != r2.P95MS || r1.MeanMS != r2.MeanMS || r1.MaxAbsMS != r2.MaxAbsMS {
		t.Fatalf("non-deterministic reports: %+v vs %+v", r1, r2)
	}
}

func TestDriftOverThresholdFails(t *testing.T) {
	m := NewClockMapping(0, 0)
	a := NewAligner(m, 20*time.Millisecond)
	// 25 ms audio lead exceeds the threshold
	videoTick := media.FromDuration(time.Second)
	audioSample := m.TickToAudio(videoTick) + uint64(float64(AudioClockRate)*0.025)
	if err := a.Observe(videoTick, audioSample); err != nil {
		t.Fatal(err)
	}
	r := a.Report()
	if !r.Failed || r.Failures != 1 || r.MaxAbsMS < 24.9 {
		t.Fatalf("expected sync failure, got %+v", r)
	}
}

func TestAudioTickOverflowRejected(t *testing.T) {
	m := NewClockMapping(0, 0)
	if _, err := m.AudioToTick(1 << 63); !media.IsCode(err, media.CodeTimestampOverflow) {
		t.Fatalf("overflow err = %v", err)
	}
}
