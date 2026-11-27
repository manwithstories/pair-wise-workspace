package stream

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
)

func testDigest(t *testing.T) string {
	t.Helper()
	b := make([]byte, 64)
	for i := range b {
		b[i] = "0123456789abcdef"[i%16]
	}
	return string(b)
}

func TestCheckpointValidation(t *testing.T) {
	dg := testDigest(t)
	good := &Checkpoint{Version: 1, Digest: dg, SegmentSizeUS: 1000, Complete: 2, NextIndex: 2, Digests: []string{"a", "b"}}
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	cases := []*Checkpoint{
		{Version: 2, Digest: dg, SegmentSizeUS: 1000},
		{Version: 1, Digest: "short", SegmentSizeUS: 1000},
		{Version: 1, Digest: dg, SegmentSizeUS: 0},
		{Version: 1, Digest: dg, SegmentSizeUS: 1000, Complete: 2, NextIndex: 3},
		{Version: 1, Digest: dg, SegmentSizeUS: 1000, Complete: 1, NextIndex: 1, Digests: []string{"a", "b"}},
		{Version: 1, Digest: dg, SegmentSizeUS: 1000, LastAckUS: -1},
	}
	for i, c := range cases {
		if err := c.Validate(); !errors.Is(err, ErrCheckpointMismatch) {
			t.Fatalf("case %d accepted: %v", i, err)
		}
	}
}

func TestCheckpointStoreAtomicRoundTrip(t *testing.T) {
	dir := t.TempDir()
	st, err := NewCheckpointStore(dir + "/state/ckpt.json")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Load(); !errors.Is(err, ErrCheckpointMismatch) {
		t.Fatalf("missing checkpoint: %v", err)
	}
	cp := &Checkpoint{Version: 1, Digest: testDigest(t), SegmentSizeUS: 1000, Complete: 1, NextIndex: 1, LastAckUS: 1000, Digests: []string{"abc"}}
	if err := st.Save(cp); err != nil {
		t.Fatal(err)
	}
	got, err := st.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got.Complete != 1 || got.LastAckUS != 1000 {
		t.Fatalf("roundtrip: %+v", got)
	}
	// Temp files must not survive.
	entries, _ := os.ReadDir(dir + "/state")
	for _, e := range entries {
		if e.Name() != "ckpt.json" {
			t.Fatalf("stray file %s", e.Name())
		}
	}
}

func TestCheckpointStoreRejectsCorruptFile(t *testing.T) {
	dir := t.TempDir()
	st, _ := NewCheckpointStore(dir + "/ckpt.json")
	if err := os.WriteFile(dir+"/ckpt.json", []byte("{oops"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Load(); !errors.Is(err, ErrCheckpointMismatch) {
		t.Fatalf("corrupt checkpoint accepted: %v", err)
	}
}

func TestPlanResume(t *testing.T) {
	dg := testDigest(t)
	cp := &Checkpoint{Version: 1, Digest: dg, SegmentSizeUS: 1000, Complete: 3, NextIndex: 3, LastAckUS: 3000}
	if r := PlanResume(nil, dg, 1000); r.Resumed {
		t.Fatal("nil checkpoint resumed")
	}
	r := PlanResume(cp, dg, 1000)
	if !r.Resumed || r.StartIndex != 3 || r.StartUS != 3000 || r.Skipped != 3 {
		t.Fatalf("resume plan: %+v", r)
	}
	if r := PlanResume(cp, testDigest(t)+"x", 1000); r.Resumed || r.Reason == "" {
		t.Fatalf("digest mismatch resumed: %+v", r)
	}
	if r := PlanResume(cp, dg, 500); r.Resumed {
		t.Fatal("geometry mismatch resumed")
	}
}

func TestRecoveryProducesIdenticalDigests(t *testing.T) {
	dg := testDigest(t)
	jobs := makeJobs(10, 100)
	consume := func(_ context.Context, j *Job) (string, error) {
		return fmt.Sprintf("seg-%02d-%d-%d", j.ID, j.StartUS, j.EndUS), nil
	}

	// Uninterrupted run.
	fullDir := t.TempDir()
	fullStore, _ := NewCheckpointStore(fullDir + "/ckpt.json")
	fullCK := NewCheckpointer(fullStore, dg, 1000, PlanResume(nil, dg, 1000))
	fullOut, err := RunWithRecovery(context.Background(), NewQueue(400), jobs, fullCK, PlanResume(nil, dg, 1000), consume, RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	full := fullCK.Current()
	if full.Complete != len(jobs) {
		t.Fatalf("full run complete %d", full.Complete)
	}

	// Interrupted run, then resume from the checkpoint.
	dir := t.TempDir()
	store, _ := NewCheckpointStore(dir + "/ckpt.json")
	resume0 := PlanResume(nil, dg, 1000)
	ck := NewCheckpointer(store, dg, 1000, resume0)
	_, err = RunWithRecovery(context.Background(), NewQueue(400), jobs, ck, resume0, consume, RunOptions{ConsumerDelay: 0, StopAfter: 5})
	if !errors.Is(err, ErrCancelled) {
		t.Fatalf("expected cancellation: %v", err)
	}
	mid := ck.Current()
	if mid.Complete != 5 {
		t.Fatalf("checkpoint after cancel: %d", mid.Complete)
	}

	resume1 := PlanResume(&mid, dg, 1000)
	if !resume1.Resumed || resume1.StartIndex != 5 {
		t.Fatalf("resume plan: %+v", resume1)
	}
	ck2 := NewCheckpointer(store, dg, 1000, resume1)
	out, err := RunWithRecovery(context.Background(), NewQueue(400), jobs, ck2, resume1, consume, RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if out.Processed != len(jobs) {
		t.Fatalf("resumed processed %d", out.Processed)
	}
	resumed := ck2.Current()
	if resumed.Complete != len(jobs) {
		t.Fatalf("resumed complete %d", resumed.Complete)
	}
	if err := compareDigests(full.Digests, resumed.Digests); err != nil {
		t.Fatal(err)
	}
	if fullOut.LastAckUS != out.LastAckUS {
		t.Fatalf("ack positions differ: %d vs %d", fullOut.LastAckUS, out.LastAckUS)
	}
}

func TestResumeRejectsMisalignedJobIDs(t *testing.T) {
	// A caller that hands over a sub-slice without preserving absolute IDs
	// would make Complete record digests under the wrong segment numbers.
	dg := testDigest(t)
	jobs := makeJobs(6, 100)[2:]
	r := Resume{Resumed: true, StartIndex: 2, Skipped: 2,
		Checkpoint: &Checkpoint{Version: 1, Digest: dg, SegmentSizeUS: 1000, Complete: 2, NextIndex: 2, LastAckUS: 2000}}
	ck := NewCheckpointer(&CheckpointStore{Path: t.TempDir() + "/x.json"}, dg, 1000, r)
	consume := func(_ context.Context, j *Job) (string, error) { return "d", nil }
	if _, err := RunWithRecovery(context.Background(), NewQueue(1000), jobs, ck, r, consume, RunOptions{}); err == nil {
		t.Fatal("misaligned job IDs accepted")
	}
}

func compareDigests(a, b []string) error {
	if len(a) != len(b) {
		return fmt.Errorf("digest count %d != %d", len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			return fmt.Errorf("segment %d: %s != %s", i, a[i], b[i])
		}
	}
	return nil
}

func TestRecoveryDetectsChangedInput(t *testing.T) {
	dg := testDigest(t)
	jobs := makeJobs(4, 100)
	consume := func(_ context.Context, j *Job) (string, error) { return "x", nil }
	dir := t.TempDir()
	store, _ := NewCheckpointStore(dir + "/ckpt.json")
	ck := NewCheckpointer(store, dg, 1000, PlanResume(nil, dg, 1000))
	if _, err := RunWithRecovery(context.Background(), NewQueue(400), jobs, ck, PlanResume(nil, dg, 1000), consume, RunOptions{}); err != nil {
		t.Fatal(err)
	}
	saved := ck.Current()
	// A new input digest invalidates the resume; the run restarts at 0.
	newDigest := "f" + dg[1:]
	r := PlanResume(&saved, newDigest, 1000)
	if r.Resumed {
		t.Fatal("changed input resumed")
	}
	ck2 := NewCheckpointer(store, newDigest, 1000, r)
	out, err := RunWithRecovery(context.Background(), NewQueue(400), jobs, ck2, r, consume, RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if ck2.Current().Complete != len(jobs) || out.Processed != len(jobs) {
		t.Fatalf("restart did not complete: %+v", ck2.Current())
	}
	if ck2.Current().Digest != newDigest {
		t.Fatal("new digest not recorded")
	}
}

func TestRecoveryRejectsOutOfOrderCompletion(t *testing.T) {
	dg := testDigest(t)
	ck := NewCheckpointer(&CheckpointStore{Path: t.TempDir() + "/x.json"}, dg, 1000, PlanResume(nil, dg, 1000))
	if err := ck.Complete(1, 1000, "a"); err == nil {
		t.Fatal("out-of-order completion accepted")
	}
	if err := ck.Complete(0, 1000, "a"); err != nil {
		t.Fatal(err)
	}
}

func TestRecoveryDetectsDigestChangeOnResume(t *testing.T) {
	// If a resumed run produces different bytes for a segment it re-executes,
	// the checkpoint must refuse to overwrite the previously verified digest.
	dg := testDigest(t)
	jobs := makeJobs(6, 100)
	dir := t.TempDir()
	store, _ := NewCheckpointStore(dir + "/ckpt.json")
	r0 := PlanResume(nil, dg, 1000)
	ck := NewCheckpointer(store, dg, 1000, r0)
	consume := func(_ context.Context, j *Job) (string, error) { return fmt.Sprintf("d%d", j.ID), nil }
	if _, err := RunWithRecovery(context.Background(), NewQueue(400), jobs, ck, r0, consume, RunOptions{StopAfter: 3}); !errors.Is(err, ErrCancelled) {
		t.Fatalf("cancel: %v", err)
	}
	mid := ck.Current()
	r1 := PlanResume(&mid, dg, 1000)
	if !r1.Resumed {
		t.Fatal("should resume")
	}
	// A completed segment that is replayed with different bytes must be
	// refused; this is what a corrupt re-decode of an acknowledged segment
	// looks like.
	ck.allAck = true
	if err := ck.Complete(2, jobs[2].EndUS, "DIFFERENT"); err == nil {
		t.Fatal("digest change on resume was accepted")
	}
	if err := ck.Complete(2, jobs[2].EndUS, mid.Digests[2]); err != nil {
		t.Fatalf("verified digest refused: %v", err)
	}
}
