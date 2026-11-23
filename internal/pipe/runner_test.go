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

	"archivecheck/internal/fault"
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
	return pipe.Run(context.Background(), f.work, f.tasks, f.loaded, start, opts)
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
	res, err := pipe.Run(ctx, f.work, f.tasks, f.loaded, f.start(), pipe.Options{
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

// TestFaultFailsPipelineAtParseStage drives a corrupted corpus through the real
// pipeline and asserts the process fails at the parse stage, with no checkpoint
// published and no output committed beyond the magic header.
func TestFaultFailsPipelineAtParseStage(t *testing.T) {
	work := t.TempDir()
	fl, err := testkit.WriteFaultInputs(work)
	if err != nil {
		t.Fatal(err)
	}
	// Generate a segment whose moof is valid (so probing succeeds) but whose
	// trun sample ranges exceed the mdat. Only full validation inside the
	// pipeline rejects it.
	shorts := []testkit.SampleSpec{{Duration: 3000, Size: 256, Sync: true}, {Duration: 3000, Size: 256}}
	data := testkit.BuildShortMdatFragment(1, 0, shorts)
	shortPath := filepath.Join(fl.Dir, "short-mdat.m4s")
	if err := os.WriteFile(shortPath, data, 0o644); err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(fl.Dir, "pipeline-manifest.txt")
	content := "init init.mp4\nmedia short-mdat.m4s\n"
	if err := os.WriteFile(manifestPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := pipe.LoadManifest(fl.Dir, manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := pipe.LoadInputs(m)
	if err != nil {
		t.Fatalf("probe of the short-mdat segment should succeed, got %v", err)
	}
	defer loaded.Close()
	ord, err := repair.Order(loaded.OrderItems())
	if err != nil {
		t.Fatal(err)
	}
	tasks := loaded.Tasks(ord)
	res, err := pipe.Run(context.Background(), work, tasks, loaded, loaded.StartFor(ord), pipe.Options{QueueCapacity: 2})
	if err == nil {
		t.Fatalf("corrupted corpus completed: %+v", res)
	}
	fe, ok := fault.As(err)
	if !ok {
		t.Fatalf("err = %v", err)
	}
	if fe.Kind != fault.KindOutOfBounds || fe.BoxType != "trun" {
		t.Fatalf("fault = %+v", fe)
	}
	if res.CommittedAt != 0 {
		t.Fatalf("committed %d fragments despite parse failure", res.CommittedAt)
	}
	if _, statErr := os.Stat(pipe.CheckpointPath(work)); !os.IsNotExist(statErr) {
		t.Fatalf("checkpoint published after a parse fault: %v", statErr)
	}
	info, statErr := os.Stat(pipe.OutputPath(work))
	if statErr != nil {
		t.Fatalf("output missing: %v", statErr)
	}
	if info.Size() != int64(len(verify.Magic)) {
		t.Fatalf("output size = %d, want only the %d-byte magic", info.Size(), len(verify.Magic))
	}
}

// TestBadMdatLengthRejectedAtProbe asserts an invalid declared length is an
// offset-bearing out-of-bounds fault before any output is created.
func TestBadMdatLengthRejectedAtProbe(t *testing.T) {
	work := t.TempDir()
	fl, err := testkit.WriteFaultInputs(work)
	if err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(fl.Dir, "bad-manifest.txt")
	content := "init init.mp4\nmedia badlength.m4s\n"
	if err := os.WriteFile(manifestPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := pipe.LoadManifest(fl.Dir, manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	_, err = pipe.LoadInputs(m)
	if err == nil {
		t.Fatal("bad declared mdat length accepted")
	}
	if !fault.IsKind(err, fault.KindOutOfBounds) {
		t.Fatalf("err = %v", err)
	}
}

func TestResumeRejectsCheckpointIndexMismatch(t *testing.T) {
	f := setup(t)
	if _, err := f.run(t, f.start(), pipe.Options{QueueCapacity: 4, InterruptAfter: 5}); !pipe.IsCancelled(err) {
		t.Fatal("expected interrupt cancellation")
	}
	cp, err := index.Load(pipe.CheckpointPath(f.work))
	if err != nil {
		t.Fatal(err)
	}
	// A checkpoint that claims a different index than the committed bytes is
	// incompatible even though the manifest and prefix digest still match.
	cp.Index.Entries[0].DTS++
	if err := cp.Seal(); err != nil {
		t.Fatal(err)
	}
	if err := index.Save(pipe.CheckpointPath(f.work), cp); err != nil {
		t.Fatal(err)
	}
	if _, err := pipe.ResumeStartFor(f.loaded.Manifest, cp, pipe.OutputPath(f.work)); err == nil {
		t.Fatal("mismatched checkpoint index accepted")
	}
}

func TestResumeAcceptsMatchingCheckpoint(t *testing.T) {
	f := setup(t)
	if _, err := f.run(t, f.start(), pipe.Options{QueueCapacity: 4, InterruptAfter: 5}); !pipe.IsCancelled(err) {
		t.Fatal("expected interrupt cancellation")
	}
	cp, err := index.Load(pipe.CheckpointPath(f.work))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pipe.ResumeStartFor(f.loaded.Manifest, cp, pipe.OutputPath(f.work)); err != nil {
		t.Fatalf("matching checkpoint rejected: %v", err)
	}
}

// TestMultipleInitGroupsRestoreIndependently writes two programs, each with its
// own init segment and fragments, then restores and verifies both in one run.
func TestMultipleInitGroupsRestoreIndependently(t *testing.T) {
	work := t.TempDir()
	inDir := filepath.Join(work, "in")
	if err := os.MkdirAll(inDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeGroup := func(tag string, trackID uint32, timescale uint32, seqs []uint32, bases []uint64) {
		init := testkit.BuildInit(testkit.InitSpec{TrackID: trackID, Timescale: timescale, Handler: "vide", Width: 640, Height: 360})
		if err := os.WriteFile(filepath.Join(inDir, "init-"+tag+".mp4"), init, 0o644); err != nil {
			t.Fatal(err)
		}
		for i, seq := range seqs {
			samples := []testkit.SampleSpec{
				{Duration: 1000, Size: 64, Sync: true},
				{Duration: 1000, Size: 64},
			}
			data := testkit.BuildFragment(testkit.FragmentSpec{
				Seq: seq, TrackID: trackID, BaseTime: bases[i], Samples: samples,
			})
			name := filepath.Join(inDir, "g"+tag+"-"+strings.TrimSpace(itoa(int(seq)))+".m4s")
			if err := os.WriteFile(name, data, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	writeGroup("a", 1, 90000, []uint32{1, 2, 3}, []uint64{0, 2000, 4000})
	// Group B reuses sequence numbers 1..3; grouping must keep them separate.
	writeGroup("b", 2, 48000, []uint32{1, 2, 3}, []uint64{0, 2000, 4000})
	manifest := "" +
		"init init-a.mp4\n" +
		"media ga-1.m4s\nmedia ga-2.m4s\nmedia ga-3.m4s\n" +
		"init init-b.mp4\n" +
		"media gb-1.m4s\nmedia gb-2.m4s\nmedia gb-3.m4s\n"
	manifestPath := filepath.Join(inDir, "manifest.txt")
	if err := os.WriteFile(manifestPath, []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := pipe.LoadManifest(inDir, manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := pipe.LoadInputs(m)
	if err != nil {
		t.Fatal(err)
	}
	defer loaded.Close()
	ord, err := repair.Order(loaded.OrderItems())
	if err != nil {
		t.Fatal(err)
	}
	if ord.Missing != 0 || ord.Dropped != 0 {
		t.Fatalf("ordering issues: %+v", ord.Issues)
	}
	res, err := pipe.Run(context.Background(), work, loaded.Tasks(ord), loaded, loaded.StartFor(ord), pipe.Options{QueueCapacity: 3})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Checkpoint.FragmentsTotal != 6 || len(res.Checkpoint.Index.Entries) != 12 {
		t.Fatalf("checkpoint = total %d entries %d", res.Checkpoint.FragmentsTotal, len(res.Checkpoint.Index.Entries))
	}
	tracks := map[uint32]int{}
	for _, e := range res.Checkpoint.Index.Entries {
		tracks[e.TrackID]++
	}
	if tracks[1] != 6 || tracks[2] != 6 {
		t.Fatalf("track distribution = %v", tracks)
	}
	rep, err := verify.Check(res.OutputPath, res.Checkpoint)
	if err != nil || !rep.Verified {
		t.Fatalf("verify: %v %+v", err, rep)
	}
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	var b []byte
	for v > 0 {
		b = append([]byte{byte('0' + v%10)}, b...)
		v /= 10
	}
	return string(b)
}

// TestResumeAfterCompletedRunIsNoop re-validates a finished checkpoint: the
// committed prefix of a completed output (which excludes the trailing digest)
// must still re-read cleanly, and re-running claims nothing new.
func TestResumeAfterCompletedRunIsNoop(t *testing.T) {
	f := setup(t)
	if _, err := f.run(t, f.start(), pipe.Options{QueueCapacity: 4}); err != nil {
		t.Fatalf("run: %v", err)
	}
	cp, err := index.Load(pipe.CheckpointPath(f.work))
	if err != nil {
		t.Fatal(err)
	}
	st, err := pipe.ResumeStartFor(f.loaded.Manifest, cp, pipe.OutputPath(f.work))
	if err != nil {
		t.Fatalf("completed checkpoint rejected: %v", err)
	}
	if st.Committed != cp.FragmentsTotal {
		t.Fatalf("committed %d != total %d", st.Committed, cp.FragmentsTotal)
	}
}
