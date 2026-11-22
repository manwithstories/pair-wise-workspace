package pipeline

import (
	"os"
	"path/filepath"
	"testing"

	"mediaroute/internal/fault"
	"mediaroute/internal/sim"
)

func testStream() *sim.Stream {
	return sim.Generate(sim.Options{Seed: 41, LossBurst: 7, Bandwidth: []int64{2e6, 8e5, 3e6}})
}

func TestRoundtripConsistency(t *testing.T) {
	s := testStream()
	straight, err := Run(s, Config{})
	if err != nil {
		t.Fatal(err)
	}
	crashAt := len(s.Items) * 3 / 5
	dir := t.TempDir()
	resumed, err := Run(s, Config{
		CheckpointInterval: len(s.Items) / 5,
		CrashAt:            crashAt,
		WorkDir:            dir,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resumed.RollbackItems <= 0 {
		t.Fatalf("rollback = %d, want > 0", resumed.RollbackItems)
	}
	a, b := straight.Manifest.Segments, resumed.Manifest.Segments
	if len(a) != len(b) {
		t.Fatalf("segment counts differ: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i].SHA256 != b[i].SHA256 {
			t.Fatalf("segment %d hash diverged after restart", i)
		}
	}
}

func TestAcceptanceMetrics(t *testing.T) {
	res, err := Run(testStream(), Config{})
	if err != nil {
		t.Fatal(err)
	}
	if got := len(res.Manifest.Segments); got < 12 {
		t.Fatalf("segments = %d, want >= 12", got)
	}
	if got := res.Manifest.KeyframeHitRate(); got < 0.98 {
		t.Fatalf("keyframe hit rate = %f, want >= 0.98", got)
	}
	if res.AlignP95Ns > 40_000_000 {
		t.Fatalf("align p95 = %d ns, want <= 40ms", res.AlignP95Ns)
	}
	if res.DuplicatesDropped < 1 {
		t.Fatal("no duplicates dropped")
	}
	if res.LossBurstRecovered < 1 {
		t.Fatal("loss burst not recovered")
	}
	if res.PublishRejected {
		t.Fatal("manifest rejected")
	}
	if got := res.Errs.Count(fault.CatTiming); got < 1 {
		t.Fatal("negative-duration subtitle not reported")
	}
}

func TestCorruptCheckpointFallsBack(t *testing.T) {
	s := testStream()
	dir := t.TempDir()
	eng := NewEngine(8)
	cp := NewCheckpointer(dir, len(s.Items)/5)
	for i, it := range s.Items {
		eng.Step(it)
		if err := cp.MaybeSave(eng, i+1); err != nil {
			t.Fatal(err)
		}
	}
	// Corrupt the current generation; the previous must survive.
	if err := os.WriteFile(filepath.Join(dir, "checkpoint.json"), []byte("garbage"), 0o644); err != nil {
		t.Fatal(err)
	}
	st, usedPrev, err := cp.Recover()
	if err != nil {
		t.Fatal(err)
	}
	if !usedPrev {
		t.Fatal("did not fall back to previous generation")
	}
	if st.ItemsProcessed <= 0 {
		t.Fatalf("recovered watermark = %d", st.ItemsProcessed)
	}
	// Corrupt both: recovery must fail with a checkpoint fault.
	if err := os.WriteFile(filepath.Join(dir, "checkpoint.prev.json"), []byte("garbage"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := cp.Recover(); err == nil {
		t.Fatal("both generations corrupt: recovery succeeded")
	} else if fe, ok := err.(*fault.Error); !ok || fe.Category != fault.CatCheckpoint {
		t.Fatalf("error = %v", err)
	}
}

func TestCorruptInputIsolation(t *testing.T) {
	s := sim.Generate(sim.Options{Seed: 41, LossBurst: 7, Corrupt: "truncated-box"})
	res, err := Run(s, Config{})
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Errs.Count(fault.CatParse); got < 1 {
		t.Fatal("truncated box not reported as parse error")
	}
	if len(res.IsolatedTracks) != 1 || res.IsolatedTracks[0] != "meta" {
		t.Fatalf("isolated = %v", res.IsolatedTracks)
	}
	// Independent tracks are unaffected: media segments still produced.
	if len(res.Manifest.Segments) < 12 {
		t.Fatalf("segments = %d after isolation", len(res.Manifest.Segments))
	}
	media := 0
	for _, evs := range res.SegEvents {
		for _, ev := range evs {
			if ev.Track == "video-720p" {
				media++
			}
		}
	}
	if media == 0 {
		t.Fatal("media track lost after meta isolation")
	}
}
