package metrics

import (
	"testing"
	"time"

	"relay/internal/media"
	"relay/internal/recovery"
)

func TestP95Batch(t *testing.T) {
	c := &Collector{}
	for i := 0; i < 20; i++ {
		c.RecordBatch(time.Duration(i+1) * time.Millisecond)
	}
	// 95th percentile of 1..20 ms lands at index ceil(0.95*20)-1 = 18 -> 19ms.
	if got := c.P95BatchMS(); got < 18.9 || got > 19.1 {
		t.Fatalf("p95: got %v want ~19", got)
	}
}

func TestRecoveryPctOf(t *testing.T) {
	res := &recovery.Result{MediaCount: 99, ExpectedSpan: 100}
	if got := RecoveryPctOf(res); got != 99.0 {
		t.Fatalf("recovery pct: got %v want 99", got)
	}
	if got := RecoveryPctOf(nil); got != 100 {
		t.Fatalf("nil recovery should be 100, got %v", got)
	}
}

func TestSyncMaxMS(t *testing.T) {
	frames := make([]*media.RawFrame, 5)
	for i := range frames {
		frames[i] = &media.RawFrame{
			Meta: media.FrameMeta{
				SourceID: 1, Sequence: uint64(i),
				PresentationNS: int64(i) * 33_333_333, // ~30 fps
			},
		}
	}
	// Uniform spacing: zero deviation.
	if got := SyncMaxMS(map[uint32][]*media.RawFrame{1: frames}); got != 0 {
		t.Fatalf("uniform sync: got %v want 0", got)
	}
	frames[3].Meta.PresentationNS += 1_000_000 // one 1ms jitter
	if got := SyncMaxMS(map[uint32][]*media.RawFrame{1: frames}); got < 0.99 || got > 1.01 {
		t.Fatalf("jitter sync: got %v want ~1.0", got)
	}
}

func TestReportValidateThresholds(t *testing.T) {
	good := &Report{
		Samples: 10, SyncMaxMS: 1.5, RecoveryPct: 99.9, P95BatchMS: 50,
	}
	if err := good.Validate(); err != nil {
		t.Fatalf("good report rejected: %v", err)
	}
	badSync := *good
	badSync.SyncMaxMS = 2.5
	if err := badSync.Validate(); err == nil {
		t.Fatal("sync over threshold must fail")
	}
	badRecovery := *good
	badRecovery.RecoveryPct = 99.0
	if err := badRecovery.Validate(); err == nil {
		t.Fatal("recovery under threshold must fail")
	}
	badP95 := *good
	badP95.P95BatchMS = 150
	if err := badP95.Validate(); err == nil {
		t.Fatal("p95 over threshold must fail")
	}
}

func TestDigestSamplesOrderInsensitive(t *testing.T) {
	mk := func(src uint32, seq uint64, p []byte) *media.RawFrame {
		return &media.RawFrame{Meta: media.FrameMeta{SourceID: src, Sequence: seq, Size: uint32(len(p))}, Payload: p}
	}
	a := []*media.RawFrame{mk(1, 0, []byte("x")), mk(2, 0, []byte("y"))}
	b := []*media.RawFrame{mk(2, 0, []byte("y")), mk(1, 0, []byte("x"))}
	if DigestSamples(a) != DigestSamples(b) {
		t.Fatal("digest must be independent of slice ordering")
	}
}
