package pipeline

import (
	"context"
	"testing"

	"mediapack/internal/checkpoint"
	"mediapack/internal/clock"
	"mediapack/internal/fixture"
)

func newTestStore(t *testing.T) *checkpoint.Store {
	t.Helper()
	s, err := checkpoint.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func testConfig(interrupt int) Config {
	c := DefaultConfig()
	c.QueueBytes = 256 * 1024
	c.InterruptAfterSegments = interrupt
	return c
}

func TestFullRunProducesStableManifest(t *testing.T) {
	data := fixture.Valid().Data
	out, err := NewOutputs(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store := newTestStore(t)
	m, err := Run(context.Background(), data, testConfig(0), out, store, nil)
	if err != nil {
		t.Fatalf("full run failed: %v", err)
	}
	if m.Segments < 20 {
		t.Fatalf("want >=20 segments, got %d", m.Segments)
	}
	if m.Representations < 2 {
		t.Fatalf("want >=2 representations, got %d", m.Representations)
	}
	if m.PeakBufferBytes > m.QueueLimitBytes {
		t.Fatalf("peak %d exceeded queue budget %d", m.PeakBufferBytes, m.QueueLimitBytes)
	}
	if len(m.ManifestSHA256) != 64 {
		t.Fatalf("manifest sha must be 64 hex chars, got %q", m.ManifestSHA256)
	}
	// Recompute from disk: identical checksum.
	diskSHA, err := out.RecomputeChecksum()
	if err != nil {
		t.Fatal(err)
	}
	stored, err := out.ReadChecksum()
	if err != nil {
		t.Fatal(err)
	}
	if diskSHA != stored || stored != m.ManifestSHA256 {
		t.Fatalf("checksum drift: mem=%s stored=%s disk=%s",
			m.ManifestSHA256, stored, diskSHA)
	}
}

func TestInterruptionCheckpointsBoundary(t *testing.T) {
	data := fixture.Valid().Data
	out, _ := NewOutputs(t.TempDir())
	store := newTestStore(t)
	cfg := testConfig(3)
	m, err := Run(context.Background(), data, cfg, out, store, nil)
	if err != nil && !IsInterrupted(err) {
		t.Fatalf("unexpected run error: %v", err)
	}
	if !store.Exists() {
		t.Fatal("interruption must leave a checkpoint behind")
	}
	st, err := store.Load()
	if err != nil {
		t.Fatalf("checkpoint after interrupt unreadable: %v", err)
	}
	if st.SourceFP == "" || st.ParseOffset <= 0 {
		t.Fatalf("checkpoint must record fingerprint and offset: %+v", st)
	}
	_ = m
}

func TestRecoveryCompletesAndMatchesChecksum(t *testing.T) {
	data := fixture.Valid().Data
	dir := t.TempDir()
	out, _ := NewOutputs(dir)
	store, _ := checkpoint.NewStore(t.TempDir())

	// Phase 1: interrupted run leaves a checkpoint.
	cfg1 := testConfig(3)
	_, err := Run(context.Background(), data, cfg1, out, store, nil)
	if err != nil && !IsInterrupted(err) {
		t.Fatalf("phase1 error: %v", err)
	}
	st, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}

	// Phase 2: resume from the checkpoint and run to completion.
	cfg2 := testConfig(0)
	resume := &ResumeState{
		ParseOffset: st.ParseOffset, WatermarkNS: st.WatermarkNS,
		ManifestSHA: st.ManifestSHA, SourceFP: st.SourceFP,
	}
	m2, err := Run(context.Background(), data, cfg2, out, store, resume)
	if err != nil {
		t.Fatalf("recovery run failed: %v", err)
	}
	if !m2.Resumed {
		t.Fatal("metrics must flag the resumed run")
	}
	if m2.ResumeMS > 2000 {
		t.Fatalf("resume took %dms, want <=2000", m2.ResumeMS)
	}

	// Phase 3: a clean full run must yield the identical checksum.
	out2, _ := NewOutputs(t.TempDir())
	store2, _ := checkpoint.NewStore(t.TempDir())
	m3, err := Run(context.Background(), data, testConfig(0), out2, store2, nil)
	if err != nil {
		t.Fatal(err)
	}
	if m2.ManifestSHA256 != m3.ManifestSHA256 {
		t.Fatalf("recovered manifest %s != clean manifest %s",
			m2.ManifestSHA256, m3.ManifestSHA256)
	}
}

func TestRecoveryRejectsChangedSource(t *testing.T) {
	data := fixture.Valid().Data
	out, _ := NewOutputs(t.TempDir())
	store := newTestStore(t)
	if _, err := Run(context.Background(), data, testConfig(3), out, store, nil); err != nil &&
		!IsInterrupted(err) {
		t.Fatal(err)
	}
	st, _ := store.Load()
	// Tamper: present a different source fingerprint at resume.
	resume := &ResumeState{
		ParseOffset: st.ParseOffset, WatermarkNS: st.WatermarkNS,
		ManifestSHA: st.ManifestSHA, SourceFP: st.SourceFP,
	}
	other := append(append([]byte(nil), data...), 0x01)
	_, err := Run(context.Background(), other, testConfig(0), out, store, resume)
	me, ok := clock.AsMediaError(err)
	if !ok || me.Category != clock.CatCheckpoint {
		t.Fatalf("changed source must be rejected with CatCheckpoint, got %v", err)
	}
}

func TestCorruptInputReportsPreciseOffset(t *testing.T) {
	f := fixture.UnknownLengthTruncated()
	out, _ := NewOutputs(t.TempDir())
	store := newTestStore(t)
	m, err := Run(context.Background(), f.Data, testConfig(0), out, store, nil)
	me, ok := clock.AsMediaError(err)
	if !ok || me.Category != clock.CatTruncated {
		t.Fatalf("truncated input must yield CatTruncated, got %v", err)
	}
	if m.CorruptOffsetErrorBytes > 1 {
		t.Fatalf("corrupt offset gap %d exceeds 1-byte tolerance", m.CorruptOffsetErrorBytes)
	}
}

func TestRepeatedVerifyChecksumsIdentical(t *testing.T) {
	data := fixture.Valid().Data
	out, _ := NewOutputs(t.TempDir())
	store := newTestStore(t)
	m1, err := Run(context.Background(), data, testConfig(0), out, store, nil)
	if err != nil {
		t.Fatal(err)
	}
	m2, err := Run(context.Background(), data, testConfig(0), out, store, nil)
	if err != nil {
		t.Fatal(err)
	}
	if m1.ManifestSHA256 != m2.ManifestSHA256 {
		t.Fatal("repeated runs produced different checksums")
	}
}
