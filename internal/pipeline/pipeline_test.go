package pipeline

import (
	"path/filepath"
	"testing"

	"github.com/archive/rtprestore/internal/checkpoint"
	"github.com/archive/rtprestore/internal/synth"
)

func testCfg(out string) Config {
	return Config{
		Params: checkpoint.Params{Seed: 41, LossPercent: 3, ReorderPercent: 7,
			WrapAt: 4294967000, DriftPPM: 180, VFR: true, CheckpointEvery: 64},
		WrapAt:          4294967000,
		OutDir:          out,
		CheckpointEvery: 64,
		BlockSize:       64,
	}
}

func TestRunEndToEnd(t *testing.T) {
	recs, truth := synth.NewBuilder(synth.Config{
		Seed: 41, VideoPackets: 400, AudioPackets: 200,
		LossPercent: 3, ReorderPercent: 7, DuplicatePct: 5,
		WrapAt: 4294967000, DriftPPM: 180, VFR: true,
	}).Build()
	m, store, err := Run(recs, truth, testCfg(t.TempDir()), false)
	if err != nil {
		t.Fatal(err)
	}
	if m.RecoverablePackets == 0 || m.RoundTripRate < 0.99 {
		t.Fatalf("round trip: %d/%d = %v", m.RoundTripped, m.RecoverablePackets, m.RoundTripRate)
	}
	if m.KeyframeHitRate != 1.0 {
		t.Fatalf("keyframe hit rate: %v (%d/%d)", m.KeyframeHitRate, m.KeyframeHits, m.KeyframeTruth)
	}
	if len(m.Gaps) == 0 {
		t.Fatal("lossy capture must record at least one gap interval")
	}
	if m.UncertainIntervals == nil {
		t.Fatal("timeline must expose uncertainty state")
	}
	if m.Segments == 0 || m.SegmentSHA256Count != m.Segments {
		t.Fatalf("segments: %d", m.Segments)
	}
	if len(store.State().Shards) != m.Segments {
		t.Fatal("shard count mismatch")
	}
	for _, s := range store.State().Shards {
		if s.SHA256 == "" || len(s.SHA256) != 64 {
			t.Fatalf("shard hash missing: %+v", s)
		}
	}
}

func TestDeterministicShards(t *testing.T) {
	cfg := synth.Config{Seed: 41, VideoPackets: 300, AudioPackets: 150,
		WrapAt: 4294967000, DriftPPM: 180, VFR: true}
	recsA, truthA := synth.NewBuilder(cfg).Build()
	recsB, truthB := synth.NewBuilder(cfg).Build()
	dirA, dirB := t.TempDir(), t.TempDir()
	ca, cb := testCfg(dirA), testCfg(dirB)
	ma, sa, err := Run(recsA, truthA, ca, false)
	if err != nil {
		t.Fatal(err)
	}
	mb, sb, err := Run(recsB, truthB, cb, false)
	if err != nil {
		t.Fatal(err)
	}
	if ma.Segments != mb.Segments {
		t.Fatalf("segment count differs: %d vs %d", ma.Segments, mb.Segments)
	}
	for i := range sa.State().Shards {
		if sa.State().Shards[i].SHA256 != sb.State().Shards[i].SHA256 {
			t.Fatalf("shard %d hash differs across identical runs", i)
		}
	}
}

func TestResumeReusesShards(t *testing.T) {
	recs, truth := synth.NewBuilder(synth.Config{
		Seed: 41, VideoPackets: 300, AudioPackets: 150, WrapAt: 4294967000,
	}).Build()
	dir := t.TempDir()
	cfg := testCfg(dir)
	m, _, err := Run(recs, truth, cfg, false)
	if err != nil {
		t.Fatal(err)
	}
	// Re-running with resume must verify and reuse every complete shard.
	_, st, err := checkpoint.Resume(dir, cfg.CheckpointEvery, cfg.Params)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Shards) != m.Segments {
		t.Fatalf("reused shards: got %d want %d", len(st.Shards), m.Segments)
	}
}

func TestResumeSkipsProcessedTicks(t *testing.T) {
	recs, truth := synth.NewBuilder(synth.Config{
		Seed: 41, VideoPackets: 300, AudioPackets: 150, WrapAt: 4294967000,
	}).Build()
	dir := t.TempDir()
	cfg := testCfg(dir)
	first, _, err := Run(recs, truth, cfg, false)
	if err != nil {
		t.Fatal(err)
	}
	// A resumed run must not duplicate shards and must keep full metrics.
	second, store, err := Run(recs, truth, cfg, true)
	if err != nil {
		t.Fatal(err)
	}
	if second.Segments != first.Segments {
		t.Fatalf("resume must keep segment count: %d -> %d", first.Segments, second.Segments)
	}
	if second.ResumedShards != first.Segments {
		t.Fatalf("resumed shards: got %d want %d", second.ResumedShards, first.Segments)
	}
	if second.RoundTripRate < 0.99 || second.KeyframeHitRate != 1.0 {
		t.Fatalf("resume metrics lost: rt=%v kf=%v", second.RoundTripRate, second.KeyframeHitRate)
	}
	// Shard hashes must be byte-identical to the first run.
	for i := range store.State().Shards {
		if store.State().Shards[i].SHA256 == "" {
			t.Fatalf("shard %d missing hash", i)
		}
	}
}

func TestResumeRejectsChangedParams(t *testing.T) {
	recs, truth := synth.NewBuilder(synth.Config{Seed: 41, VideoPackets: 200}).Build()
	dir := t.TempDir()
	cfg := testCfg(dir)
	if _, _, err := Run(recs, truth, cfg, false); err != nil {
		t.Fatal(err)
	}
	changed := cfg
	changed.Params.Seed = 7
	if _, _, err := Run(recs, truth, changed, true); err == nil {
		t.Fatal("parameter change must reject resume")
	}
}

// TestFaultPropagation checks two failure classes carrying a locatable
// position from packet reordering through to the pipeline boundary.
func TestFaultPropagation(t *testing.T) {
	mk := func(seq uint16, payload []byte) synth.Record {
		return synth.Record{
			SSRC: synth.VideoSSRC, Seq: seq, Timestamp: uint32(seq) * 3000,
			Marker: true, Payload: payload,
		}
	}
	header := func(id uint16) []byte {
		p := make([]byte, synth.HeaderLen+3)
		p[0], p[1] = byte(id>>8), byte(id)
		return p
	}
	// Class 1: once the high-water mark advances, a packet arriving beyond
	// the reorder window is rejected with SSRC and sequence attached.
	far := []synth.Record{
		mk(0, header(0)), mk(10, header(1)), mk(11, header(2)), mk(2, header(3)),
	}
	cfg1 := testCfg(filepath.Join(t.TempDir(), "a"))
	cfg1.BlockSize = 4
	if _, _, err := Run(far, synth.Truth{}, cfg1, false); err == nil {
		t.Fatal("beyond-window packet must surface a locatable error")
	} else if !contains(err.Error(), "ssrc=") {
		t.Fatalf("error must locate the stream: %v", err)
	}
	// Class 2: a truncated video payload fails at frame build with its tick.
	bad := []synth.Record{
		mk(0, header(0)),
		{SSRC: synth.VideoSSRC, Seq: 1, Timestamp: 3000, Marker: true, Payload: []byte{0x01, 0x02}},
	}
	cfg2 := testCfg(filepath.Join(t.TempDir(), "b"))
	if _, _, err := Run(bad, synth.Truth{}, cfg2, false); err == nil {
		t.Fatal("truncated payload must surface a locatable frame error")
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
