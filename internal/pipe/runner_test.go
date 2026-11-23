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
	"archivecheck/internal/repair"
	"archivecheck/internal/testkit"
	"archivecheck/internal/verify"
)

type fixture struct {
	work   string
	loaded *pipe.LoadedInput
	ord    *repair.Ordered
	tasks  []pipe.Task
}

func setup(t *testing.T) *fixture {
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
	ord, err := repair.Order(loaded.OrderItems())
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{work: work, loaded: loaded, ord: ord, tasks: loaded.Tasks(ord)}
}

func (f *fixture) start() pipe.Start { return f.loaded.StartFor(f.ord) }

func (f *fixture) run(t *testing.T, start pipe.Start, opts pipe.Options) (*pipe.Result, error) {
	t.Helper()
	return pipe.Run(context.Background(), f.work, f.loaded.Inits[0], f.tasks, f.loaded, start, opts)
}

func TestRunCompleteAndVerify(t *testing.T) {
	f := setup(t)
	res, err := f.run(t, f.start(), pipe.Options{QueueCapacity: 4})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Checkpoint.FragmentsTotal != 12 || res.Checkpoint.Committed != 12 {
		t.Fatalf("checkpoint = %+v", res.Checkpoint)
	}
	if len(res.Checkpoint.Index.Entries) != 48 {
		t.Fatalf("entries = %d", len(res.Checkpoint.Index.Entries))
	}
	if res.PeakQueue > 4 {
		t.Fatalf("peak queue = %d > capacity 4", res.PeakQueue)
	}
	rep, err := verify.Check(res.OutputPath, res.Checkpoint)
	if err != nil || !rep.Verified {
		t.Fatalf("verify: %v %+v", err, rep)
	}
	if rep.TimestampRegressions != 0 || rep.InvalidIndexOffsets != 0 {
		t.Fatalf("report = %+v", rep)
	}
}

func TestRunDeterministicAcrossRuns(t *testing.T) {
	f1 := setup(t)
	f2 := setup(t)
	r1, err := f1.run(t, f1.start(), pipe.Options{QueueCapacity: 4})
	if err != nil {
		t.Fatal(err)
	}
	r2, err := f2.run(t, f2.start(), pipe.Options{QueueCapacity: 4})
	if err != nil {
		t.Fatal(err)
	}
	a, _ := os.ReadFile(r1.OutputPath)
	b, _ := os.ReadFile(r2.OutputPath)
	sumA := sha256.Sum256(a)
	sumB := sha256.Sum256(b)
	if hex.EncodeToString(sumA[:]) != hex.EncodeToString(sumB[:]) {
		t.Fatalf("outputs differ")
	}
}

func TestInterruptThenResume(t *testing.T) {
	f := setup(t)
	res, err := f.run(t, f.start(), pipe.Options{
		QueueCapacity: 4, SlowConsumer: time.Millisecond, InterruptAfter: 7,
	})
	if !pipe.IsCancelled(err) {
		t.Fatalf("expected cancellation, got %v", err)
	}
	if res.Checkpoint == nil || res.CommittedAt != 7 {
		t.Fatalf("checkpoint = %+v", res)
	}
	if !strings.HasSuffix(res.Checkpoint.LastCommitted, "frag-008.m4s") {
		t.Fatalf("last committed = %q", res.Checkpoint.LastCommitted)
	}
	cp, err := index.Load(pipe.CheckpointPath(f.work))
	if err != nil {
		t.Fatal(err)
	}
	if cp.Committed != 7 {
		t.Fatalf("on-disk committed = %d", cp.Committed)
	}
	st, err := pipe.ResumeStart(f.loaded.Manifest, cp)
	if err != nil {
		t.Fatal(err)
	}
	res2, err := f.run(t, st, pipe.Options{QueueCapacity: 4})
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if res2.Checkpoint.Committed != 12 || len(res2.Checkpoint.Index.Entries) != 48 {
		t.Fatalf("resumed checkpoint = %+v", res2.Checkpoint)
	}
	rep, err := verify.Check(res2.OutputPath, res2.Checkpoint)
	if err != nil || !rep.Verified {
		t.Fatalf("verify after resume: %v %+v", err, rep)
	}
}

func TestResumeRejectsChangedInput(t *testing.T) {
	f := setup(t)
	_, err := f.run(t, f.start(), pipe.Options{QueueCapacity: 4, InterruptAfter: 5})
	if !pipe.IsCancelled(err) {
		t.Fatal("expected interrupt cancellation")
	}
	cp, err := index.Load(pipe.CheckpointPath(f.work))
	if err != nil {
		t.Fatal(err)
	}
	cp.InputDigest = strings.Repeat("0", 64)
	if _, err := pipe.ResumeStart(f.loaded.Manifest, cp); err == nil {
		t.Fatal("changed input digest accepted")
	}
}

func TestResumeDoesNotOverwriteChangedOutput(t *testing.T) {
	f := setup(t)
	_, err := f.run(t, f.start(), pipe.Options{QueueCapacity: 4, InterruptAfter: 5})
	if !pipe.IsCancelled(err) {
		t.Fatal("expected interrupt cancellation")
	}
	cp, err := index.Load(pipe.CheckpointPath(f.work))
	if err != nil {
		t.Fatal(err)
	}
	// Corrupt a committed byte in the output; resume must refuse to touch it.
	out, err := os.OpenFile(pipe.OutputPath(f.work), os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := out.WriteAt([]byte{0xff}, 20); err != nil {
		t.Fatal(err)
	}
	out.Close()
	st, err := pipe.ResumeStart(f.loaded.Manifest, cp)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.run(t, st, pipe.Options{QueueCapacity: 4}); err == nil {
		t.Fatal("resume overwrote output with a mismatched committed prefix")
	}
}

func TestPeakQueueBoundedAndCancelReleases(t *testing.T) {
	f := setup(t)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(3 * time.Millisecond)
		cancel()
	}()
	res, err := pipe.Run(ctx, f.work, f.loaded.Inits[0], f.tasks, f.loaded, f.start(), pipe.Options{
		QueueCapacity: 2, SlowConsumer: 3 * time.Millisecond,
	})
	if err == nil {
		return // run finished before cancellation; timing-dependent
	}
	if !pipe.IsCancelled(err) {
		t.Fatalf("run: %v", err)
	}
	if res.PeakQueue > 2 {
		t.Fatalf("peak queue = %d > 2", res.PeakQueue)
	}
}
