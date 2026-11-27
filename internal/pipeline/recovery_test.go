package pipeline_test

import (
	"errors"
	"testing"

	"pairwise/ingestcheck/internal/clock"
	"pairwise/ingestcheck/internal/fixture"
	"pairwise/ingestcheck/internal/mp4"
	"pairwise/ingestcheck/internal/pipeline"
	"pairwise/ingestcheck/internal/recovery"
	"pairwise/ingestcheck/internal/track"
)

func mustRunner(t *testing.T) *pipeline.Runner {
	t.Helper()
	r, err := pipeline.New(pipeline.DefaultConfig())
	if err != nil {
		t.Fatalf("New runner: %v", err)
	}
	return r
}

func TestHealthyPairIsLossless(t *testing.T) {
	r := mustRunner(t)
	rep, err := r.Run(fixture.Pair())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.RoundTripOK != rep.RoundTripTotal || rep.RoundTripTotal != 12 {
		t.Fatalf("roundtrip = %d/%d, want 12/12", rep.RoundTripOK, rep.RoundTripTotal)
	}
	if rep.SegmentSHAOK != rep.SegmentSHATotal || rep.SegmentSHATotal != 12 {
		t.Fatalf("segment sha = %d/%d, want 12/12", rep.SegmentSHAOK, rep.SegmentSHATotal)
	}
	if rep.KeyframeHits != rep.KeyframeTotal || rep.KeyframeTotal == 0 {
		t.Fatalf("keyframe hit = %d/%d", rep.KeyframeHits, rep.KeyframeTotal)
	}
	if rep.Isolated != 0 {
		t.Fatalf("healthy sample isolated %d fragments", rep.Isolated)
	}
	if rep.SkewP95Ms > 20 {
		t.Fatalf("p95 skew = %.2f ms exceeds 20 ms target", rep.SkewP95Ms)
	}
	if rep.Unsynchronized != 0 {
		t.Fatalf("healthy pair reported %d unsynchronised alignments", rep.Unsynchronized)
	}
	if rep.DuplicateDrop != 0 || rep.ExplicitGap != 0 {
		t.Fatalf("healthy pair has drop=%d gap=%d", rep.DuplicateDrop, rep.ExplicitGap)
	}
}

func TestRepeatRunsHashIdentical(t *testing.T) {
	r := mustRunner(t)
	first, err := r.Run(fixture.Pair())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	second, err := r.Run(fixture.Pair())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if first.AuditRoot != second.AuditRoot {
		t.Fatalf("audit root drifted: %s vs %s", first.AuditRoot, second.AuditRoot)
	}
	if len(first.SegmentHashes) != len(second.SegmentHashes) {
		t.Fatal("segment count differs across runs")
	}
	for i := range first.SegmentHashes {
		if first.SegmentHashes[i] != second.SegmentHashes[i] {
			t.Fatalf("segment %d hash differs across runs", i)
		}
	}
}

func TestTruncatedFragmentIsolated(t *testing.T) {
	r := mustRunner(t)
	rep, err := r.Run(fixture.Truncated())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Isolated == 0 || rep.FirstBadByte <= 0 {
		t.Fatalf("truncated fragments not isolated: isolated=%d first_bad=%d", rep.Isolated, rep.FirstBadByte)
	}
	// The healthy fragments must still round-trip and hash after isolation.
	if rep.RoundTripOK != rep.RoundTripTotal || rep.RoundTripOK == 0 {
		t.Fatalf("roundtrip made no progress after isolation: %d/%d", rep.RoundTripOK, rep.RoundTripTotal)
	}
	if rep.SegmentSHAOK != rep.SegmentSHATotal || rep.SegmentSHATotal == 0 {
		t.Fatalf("segments failed to hash after isolation: %d/%d", rep.SegmentSHAOK, rep.SegmentSHATotal)
	}
}

func TestRegressionOpensEpoch(t *testing.T) {
	r := mustRunner(t)
	rep, err := r.Run(fixture.Regress())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	var epochs int
	for _, tr := range rep.Tracks {
		if tr.TrackID == fixture.AudioTrackID {
			epochs = tr.Epochs
		}
	}
	if epochs == 0 {
		t.Fatal("tfdt regression did not open a new epoch")
	}
	// Raw timestamps are never rewritten, so the fragment still hashes and
	// parses exactly as it was provided.
	if rep.SegmentSHATotal != rep.SegmentSHAOK {
		t.Fatalf("regressed segments failed hashing: %d/%d", rep.SegmentSHAOK, rep.SegmentSHATotal)
	}
}

func TestBurstProducesRecoveryAndExplicitGap(t *testing.T) {
	r := mustRunner(t)
	rep, err := r.Run(fixture.Burst())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.DuplicateDrop == 0 {
		t.Fatal("burst replay was not deduplicated")
	}
	if rep.Recovered == 0 {
		t.Fatal("small packet gap was not recovered")
	}
	if rep.ExplicitGap == 0 {
		t.Fatal("wide mfhd hole was not marked explicit")
	}
	// A gap must never be silently stitched: the header gap stays visible.
	var hdr int
	for _, tr := range rep.Tracks {
		hdr += tr.HeaderGaps
	}
	if hdr == 0 {
		t.Fatal("mfhd hole not propagated as a header gap")
	}
}

func TestVectorSampleDedupsAndTracksLate(t *testing.T) {
	r := mustRunner(t)
	rep, err := r.Run(fixture.Wrap())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.DuplicateDrop == 0 {
		t.Fatal("duplicate across wrap not dropped")
	}
	var late int
	for _, tr := range rep.Tracks {
		late += tr.Late
	}
	if late == 0 {
		t.Fatal("late packet outside the window not recorded")
	}
}

func TestCycleRejected(t *testing.T) {
	_, err := track.Build([]track.Node{
		{ID: 1, Kind: track.Video, Name: "v", Depends: []uint32{2}},
		{ID: 2, Kind: track.Audio, Name: "a", Depends: []uint32{1}},
	})
	if !errors.Is(err, clock.New(clock.CodeTrackCycle, "", "")) {
		var ce *clock.Error
		if !errors.As(err, &ce) || ce.Code != clock.CodeTrackCycle {
			t.Fatalf("cycle not rejected: %v", err)
		}
	}
}

func TestMissingDependencyRejected(t *testing.T) {
	if _, err := track.Build([]track.Node{
		{ID: 2, Kind: track.Audio, Name: "a", Depends: []uint32{9}},
	}); err == nil {
		t.Fatal("dangling dependency accepted")
	}
}

func TestTopoOrderDependenciesFirst(t *testing.T) {
	g, err := track.Build([]track.Node{
		{ID: 3, Kind: track.Audio, Name: "a", Depends: []uint32{1, 2}},
		{ID: 1, Kind: track.Video, Name: "v"},
		{ID: 2, Kind: track.Video, Name: "v2"},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	order := g.Order()
	if len(order) != 3 || order[2] != 3 {
		t.Fatalf("order = %v, want 3 last", order)
	}
}

func TestRecoveryBudgetBounded(t *testing.T) {
	c := recovery.DefaultConfig()
	m, err := recovery.NewMeter(c)
	if err != nil {
		t.Fatalf("NewMeter: %v", err)
	}
	plan := m.Decide(1 << 30) // a huge gap
	if plan.Attempts > c.MaxAttempts {
		t.Fatalf("attempts = %d exceeds cap %d", plan.Attempts, c.MaxAttempts)
	}
	if plan.BudgetMs > int64(c.CostMs)*int64(c.MaxAttempts) {
		t.Fatalf("budget %d ms exceeds cap", plan.BudgetMs)
	}
	if plan.MaxWait() > recovery.MaxWaitMs {
		t.Fatalf("wait %d ms exceeds cap", plan.MaxWait())
	}
	// A re-fetch must never exceed the attempt cap.
	outcome, _, tries := m.Gap(1<<30, 1)
	if outcome != recovery.ExplicitGap {
		t.Fatalf("huge gap outcome = %v, want explicit", outcome)
	}
	if tries > c.MaxAttempts {
		t.Fatalf("tries = %d exceeds cap", tries)
	}
}

func TestBandwidthEWMAMovesGradually(t *testing.T) {
	c := recovery.DefaultConfig()
	m, _ := recovery.NewMeter(c)
	m.Observe(4.0)
	first := m.Estimate()
	m.Observe(4.0)
	second := m.Estimate()
	if first != 4.0 {
		t.Fatalf("first sample not adopted: %v", first)
	}
	if second != 4.0 {
		t.Fatalf("steady sample moved estimate: %v", second)
	}
	m.Observe(1.0)
	if m.Estimate() >= second {
		t.Fatalf("EWMA did not react to a lower sample: %v", m.Estimate())
	}
	// Non-finite and out-of-range samples are clamped or ignored.
	m.Observe(-5)
	if m.Estimate() < c.MinMbps {
		t.Fatalf("estimate fell below floor: %v", m.Estimate())
	}
}

func TestBoxParserRejectsTruncatedAndOversize(t *testing.T) {
	cfg := mp4.DefaultConfig()
	if _, err := mp4.Parse([]byte{0, 0, 0, 4}, cfg); err == nil {
		t.Fatal("short header accepted")
	}
	// Declared size far beyond the buffer.
	bad := []byte{0x00, 0x00, 0x10, 0x00, 'm', 'o', 'o', 'f'}
	if _, err := mp4.Parse(bad, cfg); err == nil {
		t.Fatal("oversized box accepted")
	}
	// A 64-bit size whose high word overflows int64.
	wide := []byte{0x00, 0x00, 0x00, 0x01, 'm', 'o', 'o', 'f', 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF}
	if _, err := mp4.Parse(wide, cfg); err == nil {
		t.Fatal("64-bit overflow accepted")
	}
}

func TestBoxUnknownRoundTrips(t *testing.T) {
	cfg := mp4.DefaultConfig()
	unknown := mp4.Marshal("uuid", []byte{1, 2, 3, 4, 5})
	boxes, err := mp4.Parse(unknown, cfg)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(boxes) != 1 || boxes[0].Type != "uuid" {
		t.Fatalf("unknown box not parsed: %+v", boxes)
	}
	if got := string(boxes[0].Bytes()); got != string(unknown) {
		t.Fatalf("unknown box changed on round trip: %x vs %x", got, unknown)
	}
}

func TestInvalidRunnerConfig(t *testing.T) {
	cfg := pipeline.DefaultConfig()
	cfg.MaxFrags = 0
	if _, err := pipeline.New(cfg); err == nil {
		t.Fatal("zero MaxFrags accepted")
	}
	cfg = pipeline.DefaultConfig()
	cfg.MaxDigests = 0
	if _, err := pipeline.New(cfg); err == nil {
		t.Fatal("zero MaxDigests accepted")
	}
}
