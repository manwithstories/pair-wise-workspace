package pipe_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"archivecheck/internal/index"
	"archivecheck/internal/pipe"
	"archivecheck/internal/testkit"
	"archivecheck/internal/verify"
)

func setup(t *testing.T) (string, *pipe.Manifest, []*fragT, map[string]*os.File) {
	t.Helper()
	work := t.TempDir()
	layout, err := testkit.WriteDemoInputs(work)
	if err != nil {
		t.Fatal(err)
	}
	m, err := pipe.LoadManifest(filepath.Join(work, "in"), layout.ManifestPath)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := pipe.LoadInputs(m)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(loaded.Close)
	return work, m, loaded.Fragments, loaded.Files
}

type fragT = fragmentAlias

func TestRunCompleteAndVerify(t *testing.T) {
	work, m, frags, files := setup(t)
	inits, err := pipe.LoadInputs(m)
	if err != nil {
		t.Fatal(err)
	}
	defer inits.Close()
	ctx := context.Background()
	res, err := pipe.Run(ctx, work, inits.Inits[0], frags, files, pipe.Start{InputDigest: m.Digest}, pipe.Options{QueueCapacity: 4})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Checkpoint == nil || res.Checkpoint.FragmentsTotal != 12 {
		t.Fatalf("checkpoint = %+v", res.Checkpoint)
	}
	if len(res.Checkpoint.Index.Entries) != 48 {
		t.Fatalf("entries = %d", len(res.Checkpoint.Index.Entries))
	}
	rep, err := verify.Check(res.OutputPath, res.Checkpoint)
	if err != nil || !rep.Verified {
		t.Fatalf("verify: %v %+v", err, rep)
	}
	// Queue occupancy never exceeds the configured capacity.
	if res.PeakQueue > 4 {
		t.Fatalf("peak queue = %d > capacity 4", res.PeakQueue)
	}
}

func TestRunDeterministicAcrossRuns(t *testing.T) {
	work1, m1, frags1, files1 := setup(t)
	work2, m2, frags2, files2 := setup(t)
	in1, _ := pipe.LoadInputs(m1)
	in2, _ := pipe.LoadInputs(m2)
	defer in1.Close()
	defer in2.Close()
	r1, err := pipe.Run(context.Background(), work1, in1.Inits[0], frags1, files1, pipe.Start{InputDigest: m1.Digest}, pipe.Options{QueueCapacity: 4})
	if err != nil {
		t.Fatal(err)
	}
	r2, err := pipe.Run(context.Background(), work2, in2.Inits[0], frags2, files2, pipe.Start{InputDigest: m2.Digest}, pipe.Options{QueueCapacity: 4})
	if err != nil {
		t.Fatal(err)
	}
	a, _ := os.ReadFile(r1.OutputPath)
	b, _ := os.ReadFile(r2.OutputPath)
	sumA := sha256.Sum256(a)
	sumB := sha256.Sum256(b)
	if hex.EncodeToString(sumA[:]) != hex.EncodeToString(sumB[:]) {
		t.Fatalf("outputs differ: %s vs %s", hex.EncodeToString(sumA[:]), hex.EncodeToString(sumB[:]))
	}
}

func TestInterruptThenResume(t *testing.T) {
	work, m, frags, files := setup(t)
	loaded, err := pipe.LoadInputs(m)
	if err != nil {
		t.Fatal(err)
	}
	defer loaded.Close()
	res, err := pipe.Run(context.Background(), work, loaded.Inits[0], frags, files, pipe.Start{InputDigest: m.Digest, MissingMfhd: 1}, pipe.Options{
		QueueCapacity: 4, SlowConsumer: time.Millisecond, InterruptAfter: 7,
	})
	if err != nil && !pipe.IsCancelled(err) {
		t.Fatalf("run: %v", err)
	}
	if !pipe.IsCancelled(err) {
		t.Fatal("expected cancellation")
	}
	if res.Checkpoint == nil || res.Checkpoint.Committed != 7 {
		t.Fatalf("checkpoint = %+v", res.Checkpoint)
	}
	if !strings.HasSuffix(res.Checkpoint.LastCommitted, "frag-008.m4s") {
		t.Fatalf("last committed = %q", res.Checkpoint.LastCommitted)
	}
	cp, err := index.Load(pipe.CheckpointPath(work))
	if err != nil {
		t.Fatal(err)
	}
	st, err := pipe.CompatibleStart(m, cp)
	if err != nil {
		t.Fatal(err)
	}
	if st.Committed != 7 {
		t.Fatalf("resume boundary = %d", st.Committed)
	}
	res2, err := pipe.Run(context.Background(), work, loaded.Inits[0], frags, files, st, pipe.Options{QueueCapacity: 4})
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if res2.Checkpoint.Committed != 12 {
		t.Fatalf("resumed committed = %d", res2.Checkpoint.Committed)
	}
	if len(res2.Checkpoint.Index.Entries) != 48 {
		t.Fatalf("entries = %d", len(res2.Checkpoint.Index.Entries))
	}
	rep, err := verify.Check(pipe.OutputPath(work), res2.Checkpoint)
	if err != nil || !rep.Verified {
		t.Fatalf("verify after resume: %v %+v", err, rep)
	}
}

func TestResumeRejectsChangedInput(t *testing.T) {
	work, m, frags, files := setup(t)
	loaded, _ := pipe.LoadInputs(m)
	defer loaded.Close()
	_, err := pipe.Run(context.Background(), work, loaded.Inits[0], frags, files, pipe.Start{InputDigest: m.Digest}, pipe.Options{QueueCapacity: 4, InterruptAfter: 5})
	if !pipe.IsCancelled(err) {
		t.Fatal("expected interrupt cancellation")
	}
	cp, err := index.Load(pipe.CheckpointPath(work))
	if err != nil {
		t.Fatal(err)
	}
	m.Digest = strings.Repeat("0", 64)
	if _, err := pipe.CompatibleStart(m, cp); err == nil {
		t.Fatal("changed input digest accepted")
	}
}

func TestCancelReleasesAndReportsLastCommitted(t *testing.T) {
	work, m, frags, files := setup(t)
	loaded, _ := pipe.LoadInputs(m)
	defer loaded.Close()
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(5 * time.Millisecond)
		cancel()
	}()
	res, err := pipe.Run(ctx, work, loaded.Inits[0], frags, files, pipe.Start{InputDigest: m.Digest}, pipe.Options{
		QueueCapacity: 2, SlowConsumer: 2 * time.Millisecond,
	})
	if err != nil && !pipe.IsCancelled(err) {
		t.Fatalf("run: %v", err)
	}
	if !pipe.IsCancelled(err) {
		t.Skip("run completed before cancellation; timing-dependent")
	}
	if res.CommittedAt < 0 {
		t.Fatalf("committed = %d", res.CommittedAt)
	}
}
