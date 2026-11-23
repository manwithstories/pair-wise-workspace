package engine_test

import (
	"os"
	"path/filepath"
	"testing"

	"mediaroute/internal/engine"
	"mediaroute/internal/fault"
	"mediaroute/internal/fixture"
	"mediaroute/internal/index"
	"mediaroute/internal/probe"
)

type harness struct {
	dir  string
	spec fixture.Spec
}

func newHarness(t *testing.T, spec fixture.Spec) *harness {
	t.Helper()
	return &harness{dir: t.TempDir(), spec: spec}
}

func (h *harness) input(t *testing.T) string {
	raw, err := h.spec.BuildMP4()
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(h.dir, "asset.mp4")
	if err := os.WriteFile(p, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func scan(t *testing.T, input, indexDir string) *probe.ContainerReport {
	t.Helper()
	sr, err := engine.Scan(engine.ScanConfig{
		InputPath: input, IndexDir: indexDir, Params: index.DefaultParams(),
	})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	return sr.Report
}

// healthyStream runs a clean end-to-end stream and returns its stats.
func streamOnce(t *testing.T, h *harness, rep *probe.ContainerReport,
	interrupt int, bw []float64) *engine.StreamStats {
	t.Helper()
	cfg := engine.StreamConfig{
		Spec: h.spec, Report: rep,
		OutputPath:     filepath.Join(h.dir, "out.bin"),
		Checkpoint:     filepath.Join(h.dir, "cp.json"),
		StartSeq:       0,
		GapTimeoutNs:   50_000_000,
		InterruptAfter: interrupt,
		BandwidthMbps:  bw,
	}
	st, err := engine.Stream(cfg)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	return st
}

func TestHealthyStreamCompletesAllSamples(t *testing.T) {
	h := newHarness(t, fixture.DefaultSpec(41))
	input := h.input(t)
	rep := scan(t, input, filepath.Join(h.dir, "idx"))
	st := streamOnce(t, h, rep, 0, nil)
	if st.Committed != h.spec.SampleCount {
		t.Fatalf("committed=%d want %d", st.Committed, h.spec.SampleCount)
	}
	if len(st.DamagedSamples) != 0 {
		t.Fatalf("healthy run reported damage: %v", st.DamagedSamples)
	}
	if len(st.OutputSHA256) != 64 {
		t.Fatalf("output hash not 64 hex chars: %q", st.OutputSHA256)
	}
}

func TestResumeProducesIdenticalOutput(t *testing.T) {
	h := newHarness(t, fixture.DefaultSpec(41))
	input := h.input(t)
	rep := scan(t, input, filepath.Join(h.dir, "idx"))
	bw := []float64{6, 1, 9}

	// Interrupted first run.
	st1 := streamOnce(t, h, rep, 120, bw)
	if !st1.Interrupted || st1.Committed < 120 {
		t.Fatalf("first run did not interrupt as expected: %+v", st1)
	}
	firstHash := st1.OutputSHA256
	// Resume from checkpoint.
	st2 := streamOnce(t, h, rep, 0, bw)
	if st2.Interrupted {
		t.Fatal("resumed run should run to completion")
	}
	if st2.Committed+st1.Committed < h.spec.SampleCount {
		t.Fatalf("total committed %d < samples %d",
			st1.Committed+st2.Committed, h.spec.SampleCount)
	}
	if st2.ReplayedSamples > st2.Committed {
		t.Fatalf("replayed %d exceeds post-checkpoint commits %d",
			st2.ReplayedSamples, st2.Committed)
	}
	if st2.ElapsedNanos/1_000_000 >= 5000 {
		t.Fatalf("resume took %dms, want <5000", st2.ElapsedNanos/1_000_000)
	}
	// Independent full run for the equality check.
	h2 := newHarness(t, h.spec)
	fullInput := h2.input(t)
	fullRep := scan(t, fullInput, filepath.Join(h2.dir, "idx"))
	full := streamOnce(t, h2, fullRep, 0, bw)
	if full.OutputSHA256 != st2.OutputSHA256 {
		t.Fatalf("resumed hash %s != full hash %s (partial=%s)",
			st2.OutputSHA256, full.OutputSHA256, firstHash)
	}
}

func TestCheckpointMismatchRejectsResume(t *testing.T) {
	h := newHarness(t, fixture.DefaultSpec(41))
	input := h.input(t)
	rep := scan(t, input, filepath.Join(h.dir, "idx"))
	st1 := streamOnce(t, h, rep, 120, nil)
	if !st1.Interrupted {
		t.Fatal("expected interruption")
	}
	// Tamper with the output so the prefix-hash check must fail.
	outPath := filepath.Join(h.dir, "out.bin")
	raw, _ := os.ReadFile(outPath)
	raw[len(raw)-1] ^= 0xff
	os.WriteFile(outPath, raw, 0o600)
	_, err := engine.Stream(engine.StreamConfig{
		Spec: h.spec, Report: rep,
		OutputPath:   outPath,
		Checkpoint:   filepath.Join(h.dir, "cp.json"),
		GapTimeoutNs: 50_000_000,
	})
	if err == nil {
		t.Fatal("tampered output resumed without error")
	}
	if fault.AsError(err).Category != fault.CatCheckpointMismatch {
		t.Fatalf("category=%s want checkpoint_mismatch", fault.AsError(err).Category)
	}
	if code := fault.ExitCode(err); code != 3 {
		t.Fatalf("exit code=%d want 3", code)
	}
}

func TestBurstLossDamageLocation(t *testing.T) {
	spec := fixture.DefaultSpec(41)
	spec.DropBurstAt = 30
	spec.DropBurstN = 8
	spec.DuplicateN = 3
	spec.ReorderWin = 12
	h := newHarness(t, spec)
	input := h.input(t)
	rep := scan(t, input, filepath.Join(h.dir, "idx"))
	st := streamOnce(t, h, rep, 0, []float64{6, 1, 9})
	if len(st.Gaps) == 0 {
		t.Fatal("burst loss produced no gaps")
	}
	if len(st.DamagedSamples) == 0 {
		t.Fatal("packet gaps did not propagate to sample damage")
	}
	// The first damaged sample must be located within one sample of the
	// projection derived from the gap's time range.
	firstGap := st.Gaps[0]
	tk := rep.Tracks[0]
	expected := tk.FindSampleAt(firstGap.TimeRange.StartNanos)
	diff := st.DamagedSamples[0] - expected
	if diff < 0 {
		diff = -diff
	}
	if diff > 1 {
		t.Fatalf("damage offset error %d samples (got=%d expected=%d)",
			diff, st.DamagedSamples[0], expected)
	}
	// Damaged samples must not appear in the committed output.
	if st.Committed+len(st.DamagedSamples) > h.spec.SampleCount {
		t.Fatal("damaged samples were fabricated into output")
	}
}

func TestRepeatedRunsAreDeterministic(t *testing.T) {
	h := newHarness(t, fixture.DefaultSpec(41))
	input := h.input(t)
	rep := scan(t, input, filepath.Join(h.dir, "idx"))
	a := streamOnce(t, h, rep, 0, []float64{6, 1, 9})
	// Second scan should reuse the index.
	sr2, err := engine.Scan(engine.ScanConfig{
		InputPath: input, IndexDir: filepath.Join(h.dir, "idx"),
		Params: index.DefaultParams(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !sr2.Reused {
		t.Fatal("valid index was not reused")
	}
	b := streamOnce(t, h, sr2.Report, 0, []float64{6, 1, 9})
	if a.OutputSHA256 != b.OutputSHA256 {
		t.Fatal("repeated runs produced different output hashes")
	}
}

func TestExitCodes(t *testing.T) {
	cases := map[fault.Category]int{
		fault.CatBadArgument:          4,
		fault.CatCheckpointMismatch:   3,
		fault.CatIndexInvalid:         3,
		fault.CatTruncatedSampleTable: 2,
		fault.CatPacketGap:            2,
	}
	for cat, want := range cases {
		if got := fault.ExitCode(fault.New(cat, "x")); got != want {
			t.Fatalf("category %s -> exit %d want %d", cat, got, want)
		}
	}
}
