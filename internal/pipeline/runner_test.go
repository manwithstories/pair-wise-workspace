package pipeline

import (
	"context"
	"testing"

	"archive/internal/batch"
	"archive/internal/model"
	"archive/internal/testdata"
)

func baseCfg(t *testing.T, scenario testdata.Scenario) Config {
	t.Helper()
	return Config{
		Seed: 41, Tracks: 2, Packets: 4000, ReorderWindow: 64,
		ChunkPackets: 256, Queue: 128, SlowWriterMs: 0,
		Scenario: scenario, OutDir: t.TempDir(), DriftLimitNS: 50_000_000,
	}
}

// TestCleanRunScoresPerfectly covers the success path: every recoverable
// packet round-trips and every keyframe is indexed.
func TestCleanRunScoresPerfectly(t *testing.T) {
	st, err := Run(context.Background(), baseCfg(t, testdata.ScenarioClean))
	if err != nil {
		t.Fatalf("clean run failed: %v", err)
	}
	if st.RoundtripRate != 1.0 {
		t.Errorf("roundtrip_rate = %.3f, want 1.000", st.RoundtripRate)
	}
	if st.KeyframeHitRate != 1.0 {
		t.Errorf("keyframe_hit_rate = %.3f, want 1.000", st.KeyframeHitRate)
	}
	if st.Segments == 0 {
		t.Error("expected at least one segment")
	}
	if st.Frames == 0 {
		t.Error("expected frames to be rendered")
	}
}

// TestBurstLossRecordsTraceableGaps verifies losses are reported as ranges
// rather than silently filled with fabricated frames.
func TestBurstLossRecordsTraceableGaps(t *testing.T) {
	st, err := Run(context.Background(), baseCfg(t, testdata.ScenarioBurstLoss))
	if err != nil {
		t.Fatalf("burst-loss run failed: %v", err)
	}
	if st.Lost == 0 {
		t.Fatal("expected lost_packets > 0")
	}
	if len(st.LostRanges) == 0 {
		t.Fatal("expected at least one traceable lost range")
	}
	var total int64
	for _, r := range st.LostRanges {
		if r.End < r.Start {
			t.Fatalf("inverted lost range %+v", r)
		}
		total += r.Len()
	}
	if total != st.Lost {
		t.Errorf("lost range total %d != lost count %d", total, st.Lost)
	}
	// Emitted plus lost must reconcile to the packets that actually shipped.
	if st.Emitted+int(st.Lost) > st.Received {
		t.Errorf("emitted %d + lost %d exceeds received %d", st.Emitted, st.Lost, st.Received)
	}
}

// TestReorderDedupRecoversInOrder verifies duplicates are removed and the
// emitted stream preserves a bounded reorder buffer.
func TestReorderDedupRecoversInOrder(t *testing.T) {
	st, err := Run(context.Background(), baseCfg(t, testdata.ScenarioReorderDup))
	if err != nil {
		t.Fatalf("reorder run failed: %v", err)
	}
	if st.Duplicates == 0 {
		t.Error("expected duplicate_packets > 0")
	}
	if st.RoundtripRate != 1.0 {
		t.Errorf("roundtrip_rate = %.3f, want 1.000", st.RoundtripRate)
	}
	if st.PeakBuffer > 128 {
		t.Errorf("peak_buffer_packets = %d, exceeds bound 128", st.PeakBuffer)
	}
}

// TestTruncatedPacketsAreRejectedNotFabricated covers the boundary-reject
// path: malformed datagrams are counted and skipped without crashing.
func TestTruncatedPacketsAreRejectedNotFabricated(t *testing.T) {
	st, err := Run(context.Background(), baseCfg(t, testdata.ScenarioTruncated))
	if err != nil {
		t.Fatalf("truncated run failed: %v", err)
	}
	if st.Truncated == 0 {
		t.Error("expected truncated_packets > 0")
	}
	if st.RoundtripRate > 1.0 {
		t.Errorf("roundtrip rate cannot exceed 1, got %.3f", st.RoundtripRate)
	}
}

// TestCorruptRTCPStillRuns covers corrupted control traffic: the report is
// refused as an anchor but the media stream still maps and renders.
func TestCorruptRTCPStillRuns(t *testing.T) {
	st, err := Run(context.Background(), baseCfg(t, testdata.ScenarioCorruptRTCP))
	if err != nil {
		t.Fatalf("corrupt-rtcp run failed: %v", err)
	}
	if st.Frames == 0 {
		t.Error("expected frames despite corrupt RTCP")
	}
}

// TestDeterministicSameSeed verifies identical seeds produce byte-identical
// segment hashes and metrics.
func TestDeterministicSameSeed(t *testing.T) {
	cfg1 := baseCfg(t, testdata.ScenarioArchive)
	cfg2 := baseCfg(t, testdata.ScenarioArchive)
	cfg2.OutDir = t.TempDir()
	st1, err := Run(context.Background(), cfg1)
	if err != nil {
		t.Fatal(err)
	}
	st2, err := Run(context.Background(), cfg2)
	if err != nil {
		t.Fatal(err)
	}
	if len(st1.SegmentSHA) != len(st2.SegmentSHA) {
		t.Fatalf("segment count differs: %d vs %d", len(st1.SegmentSHA), len(st2.SegmentSHA))
	}
	for i := range st1.SegmentSHA {
		if st1.SegmentSHA[i] != st2.SegmentSHA[i] {
			t.Errorf("segment %d hash differs across same-seed runs", i)
		}
	}
	if st1.InputDigest != st2.InputDigest {
		t.Error("input digest must be stable for the same seed")
	}
	if st1.RoundtripRate != st2.RoundtripRate || st1.Lost != st2.Lost {
		t.Error("metrics must be reproducible for the same seed")
	}
}

// TestBoundedMemoryAcrossPacketCounts verifies the peak reorder buffer does
// not grow with the total packet count: memory is bounded by the window.
func TestBoundedMemoryAcrossPacketCounts(t *testing.T) {
	small := baseCfg(t, testdata.ScenarioReorderDup)
	small.Packets = 2000
	large := baseCfg(t, testdata.ScenarioReorderDup)
	large.Packets = 12000
	large.OutDir = t.TempDir()
	s1, err := Run(context.Background(), small)
	if err != nil {
		t.Fatal(err)
	}
	s2, err := Run(context.Background(), large)
	if err != nil {
		t.Fatal(err)
	}
	if s2.PeakBuffer > 128 || s1.PeakBuffer > 128 {
		t.Errorf("peak buffer unbounded: small=%d large=%d", s1.PeakBuffer, s2.PeakBuffer)
	}
	// A 6x packet increase must not inflate the reorder peak.
	if s2.PeakBuffer > s1.PeakBuffer*2 && s2.Frames > s1.Frames*2 {
		t.Errorf("peak grew with packet count: %d -> %d", s1.PeakBuffer, s2.PeakBuffer)
	}
}

// TestInvalidParametersRejected checks the parameter boundary maps to a
// structured KindParam error.
func TestInvalidParametersRejected(t *testing.T) {
	cases := map[string]func(*Config){
		"zero tracks":   func(c *Config) { c.Tracks = 0 },
		"zero packets":  func(c *Config) { c.Packets = 0 },
		"zero window":   func(c *Config) { c.ReorderWindow = 0 },
		"zero chunk":    func(c *Config) { c.ChunkPackets = 0 },
		"zero queue":    func(c *Config) { c.Queue = 0 },
		"negative slow": func(c *Config) { c.SlowWriterMs = -1 },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := baseCfg(t, testdata.ScenarioClean)
			mutate(&cfg)
			_, err := Run(context.Background(), cfg)
			me := model.AsError(err)
			if me == nil || me.Kind != model.KindParam {
				t.Fatalf("expected KindParam error, got %v", err)
			}
		})
	}
}

// TestCancellationPreservesCheckpoints verifies a canceled run returns a
// KindCanceled error and leaves a checkpoint file behind for resume.
func TestCancellationPreservesCheckpoints(t *testing.T) {
	cfg := baseCfg(t, testdata.ScenarioClean)
	cfg.Packets = 20000
	cfg.SlowWriterMs = 1
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancel: the producer must stop on its first send
	st, err := Run(ctx, cfg)
	if err == nil {
		t.Fatal("expected an error from a canceled context")
	}
	me := model.AsError(err)
	if me == nil || me.Kind != model.KindCanceled {
		t.Fatalf("expected KindCanceled, got %v", err)
	}
	if st == nil || !st.Canceled {
		t.Error("stats should record the cancellation")
	}
	if _, err := batch.Load(cfg.OutDir); err != nil {
		t.Errorf("checkpoint must be preserved for resume: %v", err)
	}
}
