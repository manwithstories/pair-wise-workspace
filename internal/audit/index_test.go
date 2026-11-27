package audit

import (
	"math"
	"testing"

	"avpipe/internal/media"
)

var rate48 = media.Rate{Num: 48000, Den: 1}

func block(start int64, frames, channels int, fill float64) media.Block {
	samples := make([]float64, frames*channels)
	for i := range samples {
		if i%2 == 0 {
			samples[i] = fill
		} else {
			samples[i] = -fill
		}
	}
	return media.Block{Track: "mix", Start: start, Rate: rate48, Channels: channels, Samples: samples}
}

func TestSegmentsAndDeterminism(t *testing.T) {
	build := func() *Index {
		x, err := New(rate48, 1)
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 5; i++ {
			if err := x.Consume(block(int64(i*2000), 2000, 1, 0.25)); err != nil {
				t.Fatal(err)
			}
		}
		x.Finalize()
		return x
	}
	a, b := build(), build()
	if len(a.Segments()) != 3 {
		t.Fatalf("segments=%d want 3 (4800+4800+400)", len(a.Segments()))
	}
	if a.Digest() != b.Digest() {
		t.Fatal("identical runs produced different digests")
	}
	// A changed sample must change the digest.
	c := build()
	c2, _ := New(rate48, 1)
	for i := 0; i < 5; i++ {
		blk := block(int64(i*2000), 2000, 1, 0.25)
		if i == 2 {
			blk.Samples[0] = 0.5
		}
		c2.Consume(blk)
	}
	c2.Finalize()
	if c.Digest() == c2.Digest() {
		t.Fatal("digest did not change with content")
	}
	segs := a.Segments()
	if segs[0].Start != 0 || segs[0].Frames != 4800 {
		t.Fatalf("segment0=%+v", segs[0])
	}
	if segs[1].Start != 4800 || segs[2].Frames != 400 {
		t.Fatalf("segments=%+v", segs)
	}
	var zero Segment
	for _, s := range segs {
		if s.SHA == zero.SHA {
			t.Fatal("empty hash")
		}
	}
}

func TestSegmentBoundaryIndependence(t *testing.T) {
	// Consuming in 100-frame chunks or 5000-frame chunks must hash identically
	// because segmentation is by cumulative frame count.
	mk := func(chunk int) string {
		x, _ := New(rate48, 1)
		samples := make([]float64, 12000)
		for i := range samples {
			samples[i] = math.Sin(float64(i) * 0.01)
		}
		for pos := 0; pos < len(samples); pos += chunk {
			end := pos + chunk
			if end > len(samples) {
				end = len(samples)
			}
			b := media.Block{Track: "mix", Start: int64(pos), Rate: rate48, Channels: 1, Samples: samples[pos:end]}
			if err := x.Consume(b); err != nil {
				t.Fatal(err)
			}
		}
		x.Finalize()
		return x.Digest()
	}
	if mk(100) != mk(5000) {
		t.Fatal("segmentation depends on block size")
	}
}

func TestKeyframes(t *testing.T) {
	x, _ := New(rate48, 1)
	x.AddKeyframe(0, "k0")
	x.AddKeyframe(4800, "k1")
	x.AddKeyframe(9600, "k2")
	x.Consume(block(0, 5000, 1, 0.1))
	x.Finalize()
	s := x.Summarize()
	if s.KeyframeTotal != 3 || s.KeyframeHits != 2 {
		t.Fatalf("hits=%d/%d", s.KeyframeHits, s.KeyframeTotal)
	}
	if math.Abs(s.KeyframePct-100*2.0/3) > 1e-9 {
		t.Fatalf("pct=%f", s.KeyframePct)
	}
	// Empty anchor set is a full hit by convention.
	y, _ := New(rate48, 1)
	y.Consume(block(0, 10, 1, 0.1))
	if y.KeyframeHitPercent() != 100 {
		t.Fatal("empty anchors not 100%")
	}
}

func TestRoundTripAlwaysExact(t *testing.T) {
	x, _ := New(rate48, 2)
	samples := make([]float64, 1000)
	for i := range samples {
		samples[i] = math.Sin(float64(i) * 0.3)
	}
	if err := x.Consume(media.Block{Track: "mix", Rate: rate48, Channels: 2, Samples: samples}); err != nil {
		t.Fatal(err)
	}
	x.Finalize()
	s := x.Summarize()
	if s.RoundTripBad != 0 || s.RoundTripPct != 100 {
		t.Fatalf("roundtrip=%+v", s)
	}
	if s.Peak > 1 || s.PeakDBFS > 0 {
		t.Fatalf("peak=%f dB=%f", s.Peak, s.PeakDBFS)
	}
	var buckets int64
	for _, v := range s.Quantized {
		buckets += v
	}
	if buckets != int64(len(samples)) {
		t.Fatalf("quantized buckets=%d want %d", buckets, len(samples))
	}
}

func TestInvalidFormatRejected(t *testing.T) {
	if _, err := New(media.Rate{}, 1); err == nil {
		t.Fatal("invalid rate accepted")
	}
	if _, err := New(rate48, 0); err == nil {
		t.Fatal("zero channels accepted")
	}
	x, _ := New(rate48, 2)
	if err := x.Consume(block(0, 10, 1, 0.1)); err == nil {
		t.Fatal("channel mismatch accepted")
	}
	if err := x.Consume(media.Block{Track: "mix", Rate: media.Rate{Num: 44100, Den: 1}, Channels: 2}); err == nil {
		t.Fatal("rate mismatch accepted")
	}
	if media.ExitFor(func() error { _, e := New(rate48, 0); return e }()) != media.ExitUsage {
		t.Fatal("bad config should be usage error")
	}
}

func TestEmptyIndex(t *testing.T) {
	x, _ := New(rate48, 1)
	x.Finalize()
	s := x.Summarize()
	if s.Frames != 0 || s.SegmentCount != 0 || s.RoundTripPct != 100 {
		t.Fatalf("summary=%+v", s)
	}
	if x.Digest() == "" {
		t.Fatal("empty digest")
	}
}
