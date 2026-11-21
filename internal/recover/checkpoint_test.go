package recover

import (
	"errors"
	"testing"

	"mediascan/internal/index"
	"mediascan/internal/model"
)

func sampleIndex() *index.Index {
	return &index.Index{
		Version:  index.Version,
		Manifest: "hls/media.m3u8",
		InputSHA: "manifest-abc",
		Entries: []index.Entry{
			{Ordinal: 0, URI: "a.m4s", SHA256: "s0", Present: true, Size: 10},
			{Ordinal: 1, URI: "b.m4s", SHA256: "s1", Present: true, Size: 20},
			{Ordinal: 2, URI: "c.m4s", SHA256: "", Present: false},
			{Ordinal: 3, URI: "d.m4s", SHA256: "s3", Present: true, Size: 40},
		},
	}
}

func defaultParams(t *testing.T) Params {
	t.Helper()
	p, err := Params{LossEvery: 17}.Normalize()
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestParamsNormalize(t *testing.T) {
	if _, err := (Params{LossEvery: 1}).Normalize(); err == nil {
		t.Fatal("loss_every=1 accepted")
	}
	if _, err := (Params{LossEvery: 17, InterruptAfter: -1}).Normalize(); err == nil {
		t.Fatal("negative interrupt accepted")
	}
	p, err := (Params{LossEvery: 17}).Normalize()
	if err != nil {
		t.Fatal(err)
	}
	if p.AVToleranceMS != 40 || p.MaxGOPMS != 2000 || p.ReorderWindow != 64 {
		t.Fatalf("defaults: %+v", p)
	}
}

func TestCheckpointValidationBindsInputs(t *testing.T) {
	idx := sampleIndex()
	params := defaultParams(t)
	cp := CheckpointForIndex(idx, params, nil)
	if cp.NextSeg != 2 {
		t.Fatalf("next segment should stop at first gap: %d", cp.NextSeg)
	}
	if err := cp.Validate(BindingOf(idx, params)); err != nil {
		t.Fatalf("fresh checkpoint rejected: %v", err)
	}

	// Changing media invalidates even with the same manifest digest.
	changed := sampleIndex()
	changed.Entries[0].SHA256 = "different"
	if err := cp.Validate(BindingOf(changed, params)); !errors.Is(err, ErrInputChanged) {
		t.Fatalf("changed media: %v", err)
	}
	// Changing the index version invalidates.
	bumped := BindingOf(idx, params)
	bumped.IndexVersion++
	if err := cp.Validate(bumped); !errors.Is(err, ErrInputChanged) {
		t.Fatalf("index version: %v", err)
	}
	// Changing analysis parameters invalidates.
	other, _ := (Params{LossEvery: 19}).Normalize()
	if err := cp.Validate(BindingOf(idx, other)); !errors.Is(err, ErrInputChanged) {
		t.Fatalf("params: %v", err)
	}
	// Nil checkpoint is never reusable.
	var nilCp *Checkpoint
	if err := nilCp.Validate(BindingOf(idx, params)); !errors.Is(err, ErrInputChanged) {
		t.Fatalf("nil checkpoint: %v", err)
	}
}

func TestResumeStopsAtFirstGap(t *testing.T) {
	idx := sampleIndex()
	params := defaultParams(t)
	cp := CheckpointForIndex(idx, params, nil)
	cp.NextSeg = 4 // pretend everything finished
	cp.Completed = 4
	entries := CompletedEntries(cp, idx)
	if len(entries) != 2 {
		t.Fatalf("resume must stop before the missing segment: %d entries", len(entries))
	}

	cp2 := CheckpointForIndex(idx, params, cp)
	if cp2.NextSeg != 4 {
		t.Fatalf("valid prior position not preserved: %d", cp2.NextSeg)
	}
}

func TestCheckpointStoreRoundTrip(t *testing.T) {
	work := t.TempDir()
	store := NewStore(work)
	if cp, err := store.Load(); err != nil || cp != nil {
		t.Fatalf("empty load: %v %v", cp, err)
	}
	idx := sampleIndex()
	params := defaultParams(t)
	cp := CheckpointForIndex(idx, params, nil)
	cp.Status = "interrupted"
	if err := store.Save(cp); err != nil {
		t.Fatal(err)
	}
	back, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if back.NextSeg != cp.NextSeg || back.Params != params || back.Status != "interrupted" {
		t.Fatalf("round trip: %+v", back)
	}
}

func TestResultDigestStable(t *testing.T) {
	idx := sampleIndex()
	a, err := ResultDigest(idx)
	if err != nil {
		t.Fatal(err)
	}
	b, err := ResultDigest(idx)
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatal("result digest is not stable")
	}
	idx.Entries[0].URI = "z.m4s"
	c, err := ResultDigest(idx)
	if err != nil {
		t.Fatal(err)
	}
	if a == c {
		t.Fatal("result digest ignored a change")
	}
}

func TestSummaryFaults(t *testing.T) {
	fs := []model.Fault{
		{Kind: model.FaultMissingSegment},
		{Kind: model.FaultMissingSegment},
		{Kind: model.FaultAVSync},
	}
	got := SummaryFaults(fs)
	if got != "missing_segment=2,av_sync=1" {
		t.Fatalf("summary %q", got)
	}
	if SummaryFaults(nil) != "none" {
		t.Fatal("empty summary")
	}
}
