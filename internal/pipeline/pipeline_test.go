package pipeline

import (
	"context"
	"testing"
	"time"

	"mediapipeline/internal/mediaerr"
	"mediapipeline/internal/testdata"
)

func baseConfig() Config {
	return Config{
		MaxBufferBytes:    1 << 20,
		MaxBufferPackets:  256,
		ReorderWindow:     64,
		ConsumerTimeoutNS: 0,
	}
}
func baseOpts() RunOptions {
	return RunOptions{
		SegmentNS:       500_000_000,
		ConsumerDelayNS: 0,
		Audio:           DefaultAudioSpec(),
		KeyframeEvery:   10,
	}
}

func runFixture(t *testing.T, spec testdata.Spec) (*Result, *mediaerr.Error) {
	t.Helper()
	r, merr := NewRunner(baseConfig(), baseOpts())
	if merr != nil {
		t.Fatal(merr)
	}
	return r.Run(context.Background(), testdata.Generate(spec))
}

func TestRunCleanFixture(t *testing.T) {
	res, merr := runFixture(t, testdata.DefaultSpec(42))
	if merr != nil {
		t.Fatalf("clean run must not fail: %v", merr)
	}
	if len(res.Segments) == 0 {
		t.Fatal("expected at least one segment")
	}
	for i, s := range res.Segments {
		if len(s.SHA256) != 64 {
			t.Fatalf("segment %d hash must be 64 hex chars, got %q", i, s.SHA256)
		}
		if s.RoundTrip != 1.0 {
			t.Fatalf("segment %d round-trip=%f want 1.0", i, s.RoundTrip)
		}
		if s.VideoSamples > 0 && s.KeyframeHitRate != 1.0 {
			t.Fatalf("segment %d keyframe hit=%f want 1.0", i, s.KeyframeHitRate)
		}
	}
	if res.Counters.Parsed == 0 {
		t.Fatal("no packets parsed")
	}
}

func TestRunDeterministicHashes(t *testing.T) {
	spec := testdata.DefaultSpec(42)
	r1, merr := runFixture(t, spec)
	if merr != nil {
		t.Fatal(merr)
	}
	r2, merr := runFixture(t, spec)
	if merr != nil {
		t.Fatal(merr)
	}
	if len(r1.Segments) != len(r2.Segments) {
		t.Fatal("segment count differs across runs")
	}
	for i := range r1.Segments {
		if r1.Segments[i].SHA256 != r2.Segments[i].SHA256 {
			t.Fatalf("segment %d hash not stable for fixed seed", i)
		}
	}
}

func TestRunCorruptionIsPartialFailure(t *testing.T) {
	spec := testdata.DefaultSpec(42)
	spec.Corruptions = []testdata.Corruption{
		{Track: spec.AudioSSRC, SeqIndex: 5, Kind: testdata.CorruptTruncated},
		{Track: 0, SeqIndex: 0, Kind: testdata.CorruptBadRTCP},
		{Track: spec.AudioSSRC, SeqIndex: 11, Kind: testdata.CorruptDuplicateConflict},
	}
	res, merr := runFixture(t, spec)
	if merr != nil {
		t.Fatalf("isolated faults must not abort the run: %v", merr)
	}
	if res.Counters.RTCPInvalid != 1 {
		t.Fatalf("bad RTCP count=%d want 1", res.Counters.RTCPInvalid)
	}
	if res.Counters.ParseErrors != 1 {
		t.Fatalf("truncated parse errors=%d want 1", res.Counters.ParseErrors)
	}
	if res.Counters.PartialFailures == 0 {
		t.Fatal("partial failures must be counted")
	}
	// Later valid segments still exist and are fully consistent.
	var healthy int
	for _, s := range res.Segments {
		if !s.PartialFailure && s.RoundTrip == 1.0 {
			healthy++
		}
	}
	if healthy == 0 {
		t.Fatal("corruption must not poison every segment")
	}
}

func TestRunSequenceWrap(t *testing.T) {
	spec := testdata.BoundarySpecs(42)["sequence_wrap"]
	res, merr := runFixture(t, spec)
	if merr != nil {
		t.Fatalf("wrap fixture failed: %v", merr)
	}
	if res.Counters.Parsed == 0 {
		t.Fatal("wrap packets must parse and process")
	}
}

func TestRunEmptyStream(t *testing.T) {
	spec := testdata.BoundarySpecs(42)["empty_stream"]
	res, merr := runFixture(t, spec)
	if merr != nil {
		t.Fatalf("empty stream must not be fatal: %v", merr)
	}
	if res.Counters.Parsed != 0 || res.Counters.RTCPValid != 1 {
		t.Fatalf("empty stream counters wrong: %+v", res.Counters)
	}
}

func TestRunSlowConsumerTimeout(t *testing.T) {
	cfg := baseConfig()
	cfg.MaxBufferPackets = 4
	cfg.MaxBufferBytes = 1 << 16
	cfg.ConsumerTimeoutNS = int64(5 * time.Millisecond)
	opts := baseOpts()
	opts.ConsumerDelayNS = int64(50 * time.Millisecond) // far slower than timeout
	r, merr := NewRunner(cfg, opts)
	if merr != nil {
		t.Fatal(merr)
	}
	_, merr = r.Run(context.Background(), testdata.Generate(testdata.DefaultSpec(42)))
	if merr == nil {
		t.Fatal("slow consumer must trigger a timeout error")
	}
	if merr.Kind != mediaerr.KindTimeout && merr.Kind != mediaerr.KindCanceled {
		t.Fatalf("expected timeout/cancel, got %v", merr)
	}
}

func TestRunExplicitCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	// A deliberately slow consumer guarantees the run is still blocked when
	// cancellation arrives, removing the timing race with a fast completion.
	cfg := baseConfig()
	cfg.MaxBufferPackets = 8
	cfg.MaxBufferBytes = 1 << 16
	opts := baseOpts()
	opts.ConsumerDelayNS = int64(2 * time.Millisecond)
	r, merr := NewRunner(cfg, opts)
	if merr != nil {
		t.Fatal(merr)
	}
	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()
	_, merr = r.Run(ctx, testdata.Generate(testdata.DefaultSpec(42)))
	if merr == nil || merr.Kind != mediaerr.KindCanceled {
		t.Fatalf("expected cancellation error, got %v", merr)
	}
}

func TestRunMemoryBoundRespected(t *testing.T) {
	cfg := baseConfig()
	cfg.MaxBufferBytes = 1 << 16 // 64 KiB total
	cfg.MaxBufferPackets = 8
	opts := baseOpts()
	opts.ConsumerDelayNS = int64(time.Millisecond) // build back pressure
	r, merr := NewRunner(cfg, opts)
	if merr != nil {
		t.Fatal(merr)
	}
	res, merr := r.Run(context.Background(), testdata.Generate(testdata.DefaultSpec(42)))
	if merr != nil {
		t.Fatalf("bounded run failed: %v", merr)
	}
	// The 20% fixed-overhead allowance from the spec.
	limit := cfg.MaxBufferBytes + cfg.MaxBufferBytes/5
	if res.PeakBufferBytes > limit {
		t.Fatalf("peak buffer %d exceeded bound %d", res.PeakBufferBytes, limit)
	}
}
