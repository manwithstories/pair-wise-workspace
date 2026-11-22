package pipeline

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"example.com/medialab/archive/internal/checkpoint"
	"example.com/medialab/archive/internal/model"
	"example.com/medialab/archive/internal/provenance"
	"example.com/medialab/archive/internal/segment"
)

func testConfig(t *testing.T) Config {
	t.Helper()
	cfg := DefaultConfig()
	cfg.DurationUS = 40_000_000
	cfg.SegmentUS = 10_000_000
	cfg.LateWindowUS = 2_000_000
	cfg.WorkDir = t.TempDir()
	cfg.Damage = DamageMixed
	return cfg
}

func runFull(t *testing.T, cfg Config) *Runner {
	t.Helper()
	r, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	r.Attach(nil, nil, nil)
	if err := r.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestConfigValidate(t *testing.T) {
	good := DefaultConfig()
	good.WorkDir = t.TempDir()
	if err := good.Validate(); err != nil {
		t.Fatalf("default rejected: %v", err)
	}
	bad := []func(*Config){
		func(c *Config) { c.DurationUS = 0 },
		func(c *Config) { c.SegmentUS = 0 },
		func(c *Config) { c.SegmentUS = c.DurationUS + 1 },
		func(c *Config) { c.LateWindowUS = c.SegmentUS + 1 },
		func(c *Config) { c.InterruptAfterUS = c.DurationUS },
		func(c *Config) { c.Seed = 0 },
		func(c *Config) { c.MaxFillUS = 0 },
		func(c *Config) { c.WorkDir = "" },
		func(c *Config) { c.Damage = "bogus" },
	}
	for i, mutate := range bad {
		c := good
		mutate(&c)
		if err := c.Validate(); err == nil {
			t.Fatalf("case %d accepted", i)
		}
	}
}

func TestParseDuration(t *testing.T) {
	cases := map[string]int64{"120s": 120e6, "2s": 2e6, "1m30s": 90e6, "500ms": 500e3}
	for in, want := range cases {
		got, err := ParseDuration(in)
		if err != nil || got != want {
			t.Fatalf("%s = %d/%v want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "abc", "-1s", "0s"} {
		if _, err := ParseDuration(bad); err == nil {
			t.Fatalf("%q accepted", bad)
		}
	}
}

func TestRunCommitsEverySegmentForEveryRep(t *testing.T) {
	cfg := testConfig(t)
	r := runFull(t, cfg)
	rep := r.BuildReport()
	want := 4 * 2
	if rep.Segments != want {
		t.Fatalf("segments = %d want %d", rep.Segments, want)
	}
	if rep.Reps != 2 {
		t.Fatalf("reps = %d", rep.Reps)
	}
	// Both representations must commit the same index range on the same grid.
	for _, st := range r.streams {
		if st.frontier != 4 {
			t.Fatalf("stream %s frontier = %d", st.spec.rep.ID, st.frontier)
		}
	}
	if rep.BoundaryGridDelta != 0 {
		t.Fatalf("grid delta = %d", rep.BoundaryGridDelta)
	}
}

func TestOutputDigestDeterministic(t *testing.T) {
	cfg := testConfig(t)
	a := runFull(t, cfg).OutputDigest()
	b := runFull(t, cfg).OutputDigest()
	if a != b || a == "" {
		t.Fatalf("digests %q/%q", a, b)
	}
	cfg.Seed = 43
	c := runFull(t, cfg).OutputDigest()
	if c == a {
		t.Fatal("different seed produced the same digest")
	}
}

func TestStopAndResumeMatchesCleanRun(t *testing.T) {
	cfg := testConfig(t)
	cfg.InterruptAfterUS = 15_000_000
	store, err := checkpoint.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	r.Attach(store, nil, nil)
	if err := r.Run(context.Background()); !errors.Is(err, ErrInterrupted) {
		t.Fatalf("first run err = %v", err)
	}
	first := r.OutputDigest()
	st, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if st.InputOffset <= 0 || st.InputOffset >= int64(len(r.log)) {
		t.Fatalf("checkpoint offset = %d", st.InputOffset)
	}
	if st.ConfigDigest == "" || len(st.CommittedSHA) == 0 {
		t.Fatalf("checkpoint missing config/sha: %+v", st)
	}

	resumeCfg := cfg
	resumeCfg.InterruptAfterUS = 0
	resumeCfg.Resume = true
	r2, err := New(resumeCfg)
	if err != nil {
		t.Fatal(err)
	}
	r2.Attach(nil, nil, nil)
	if err := r2.Restore(st); err != nil {
		t.Fatal(err)
	}
	if err := r2.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Committing the same slot again from the resumed runner is idempotent.
	before := r2.OutputDigest()
	for _, st := range r2.streams {
		r2.commitSegment(st, 0, false, model.PacketID{}, 0)
	}
	if r2.OutputDigest() != before {
		// A duplicate submission of an already-committed slot must not append.
		t.Fatalf("duplicate commit changed the digest")
	}
	if r2.OutputDigest() == first {
		t.Log("interrupted digest already complete; resume changed nothing further")
	}
	cleanCfg := cfg
	cleanCfg.InterruptAfterUS = 0
	clean := runFull(t, cleanCfg).OutputDigest()
	if clean != r2.OutputDigest() {
		t.Fatalf("resume %s != clean %s", r2.OutputDigest(), clean)
	}
}

func TestRestoreRejectsDifferentConfig(t *testing.T) {
	cfg := testConfig(t)
	r, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	other := cfg
	other.Bitrates = "64k,96k"
	r2, err := New(other)
	if err != nil {
		t.Fatal(err)
	}
	st := checkpoint.State{ConfigDigest: cfg.Digest(r.reps), Pipeline: []byte("{}")}
	if err := r2.Restore(st); err == nil {
		t.Fatal("mismatched config accepted")
	}
	if err := r.Restore(checkpoint.State{ConfigDigest: cfg.Digest(r.reps)}); err == nil {
		t.Fatal("empty pipeline state accepted")
	}
}

func TestQuarantineLateBeyondWindow(t *testing.T) {
	cfg := testConfig(t)
	// Boundary traffic is placed every third segment; use a run long enough
	// to contain several full segments after the first two.
	cfg.DurationUS = 120_000_000
	cfg.LateAtBoundary = true
	cfg.Damage = DamageBurstLoss
	r := runFull(t, cfg)
	rep := r.BuildReport()
	if rep.Quarantined == 0 {
		t.Fatal("no packets quarantined in a boundary-late scenario")
	}
	sawLate := false
	for _, q := range r.quarantine {
		if q.Kind == model.KindLate {
			sawLate = true
		}
		if q.Kind == "" {
			t.Fatalf("quarantine without a shared kind: %+v", q)
		}
	}
	if !sawLate {
		t.Fatal("expected at least one late quarantined packet")
	}
}

func TestCorruptPacketsNeverGetFabricatedDuration(t *testing.T) {
	cfg := testConfig(t)
	r := runFull(t, cfg)
	for _, st := range r.streams {
		for _, ev := range st.events {
			if ev.DurationUS <= 0 {
				t.Fatalf("zero/negative duration reached a segment: %+v", ev.ID)
			}
		}
	}
	if r.recoveryStats.CorruptHoles == 0 {
		t.Fatal("no corrupt holes detected in mixed damage")
	}
}

func TestDistinctPacketDurations(t *testing.T) {
	cfg := testConfig(t)
	r := runFull(t, cfg)
	durs := map[int64]bool{}
	for _, st := range r.streams {
		for _, ev := range st.events {
			durs[ev.DurationUS] = true
		}
	}
	if len(durs) < 2 {
		t.Fatalf("expected at least two packet durations, got %v", durs)
	}
}

func TestLateInsideWindowTriggersReplay(t *testing.T) {
	cfg := testConfig(t)
	cfg.DurationUS = 120_000_000
	cfg.LateAtBoundary = true
	r := runFull(t, cfg)
	if r.versions.ReplayedSegments() == 0 {
		t.Fatal("in-window late packets did not recalculate any segment")
	}
	if r.versions.MaxVersion() < 2 {
		t.Fatalf("max version = %d", r.versions.MaxVersion())
	}
}

func TestManifestAndLedgerFiles(t *testing.T) {
	cfg := testConfig(t)
	dir := t.TempDir()
	store, _ := checkpoint.NewStore(dir)
	mfPath := filepath.Join(dir, "manifest.jsonl")
	lgPath := filepath.Join(dir, "ledger.jsonl")
	mf, _ := os.Create(mfPath)
	lg, _ := os.Create(lgPath)
	r, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	r.Attach(store, segment.NewWriter(mf), provenance.NewWriter(lg))
	if err := r.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	mf.Close()
	lg.Close()
	for _, p := range []string{mfPath, lgPath, store.Path()} {
		if info, err := os.Stat(p); err != nil || info.Size() == 0 {
			t.Fatalf("%s missing or empty: %v", p, err)
		}
	}
	st, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if st.ManifestCount != 8 || st.ManifestChain == "" || st.LedgerDigest == "" {
		t.Fatalf("state = %+v", st)
	}
}

func TestBoundedMemoryWithLongArchive(t *testing.T) {
	cfg := testConfig(t)
	cfg.DurationUS = 300_000_000 // five minutes
	r := runFull(t, cfg)
	totalEvents := 0
	for _, st := range r.streams {
		totalEvents += len(st.events)
	}
	// The window must not retain the whole archive: five minutes at 50 and
	// 47 packets per second is tens of thousands of events.
	if totalEvents > 2500 {
		t.Fatalf("active window grew to %d events", totalEvents)
	}
	for k := range r.counted {
		if len(k) == 0 {
			t.Fatal("empty counted key")
		}
	}
}

func TestMaterializationWritesVerifiableSegments(t *testing.T) {
	cfg := testConfig(t)
	r, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	r.Attach(nil, nil, nil)
	if err := r.EnableMaterialization(); err != nil {
		t.Fatal(err)
	}
	if err := r.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := r.MaterializerError(); err != nil {
		t.Fatal(err)
	}
	rep := r.BuildReport()
	if rep.EncodedBytes <= 0 {
		t.Fatal("no media bytes written")
	}
	files, _ := filepath.Glob(filepath.Join(r.ArtefactDir(), "media", "*", "*.m4s"))
	if len(files) == 0 {
		t.Fatal("no media segments on disk")
	}
	initFiles, _ := filepath.Glob(filepath.Join(r.ArtefactDir(), "init", "*.mp4"))
	if len(initFiles) != 2 {
		t.Fatalf("init segments = %d", len(initFiles))
	}
}

func TestThroughputTarget(t *testing.T) {
	if testing.Short() {
		t.Skip("throughput check skipped in -short")
	}
	cfg := testConfig(t)
	cfg.DurationUS = 120_000_000
	cfg.Damage = DamageNone
	start := time.Now()
	runFull(t, cfg)
	elapsed := time.Since(start)
	// The brief asks for at least 20x real time for the 120s sample.
	if elapsed > 6*time.Second {
		t.Fatalf("120s sample took %v (target under 6s for 20x)", elapsed)
	}
}
