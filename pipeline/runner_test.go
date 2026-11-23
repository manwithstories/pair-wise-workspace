package pipeline

import (
	"context"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"mediad/checkpoint"
	"mediad/diag"
	"mediad/segment"
	"mediad/timeline"
	"mediad/track"
)

func demoFrames(fr timeline.FrameRate, count, keyEvery, phase int) []segment.Frame {
	out := make([]segment.Frame, 0, count)
	for i := 0; i < count; i++ {
		tt, _ := timeline.FrameToTime(fr, int64(i))
		out = append(out, segment.Frame{Time: tt, Keyframe: (i+phase)%keyEvery == 0})
	}
	return out
}

func newTestGraph(t *testing.T, reps []string) *track.Graph {
	t.Helper()
	tr := make([]track.Track, 0, len(reps)+1)
	tr = append(tr, track.Track{ID: "tc", Kind: track.KindTimecode})
	for _, id := range reps {
		tr = append(tr, track.Track{ID: id, Kind: track.KindRTP, Deps: []string{"tc"}})
	}
	g, err := track.NewGraph(tr)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

// failingWriter fails writes for a specific segment index, to simulate a disk
// failure mid-run; subsequent runs use a fresh writer and resume.
type failingWriter struct {
	failAt int32
	calls  int32
}

func (w *failingWriter) WriteSegment(rep string, idx int, payload []byte) (string, error) {
	atomic.AddInt32(&w.calls, 1)
	if int32(idx) == atomic.LoadInt32(&w.failAt) {
		return "", diag.New(diag.EOutput, rep, "simulated disk failure at segment %d", idx)
	}
	fs := FSWriter{Dir: writerDir}
	return fs.WriteSegment(rep, idx, payload)
}

var writerDir string

func TestQueueBackpressureAndPeak(t *testing.T) {
	q, err := NewBoundedQueue[int](8)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Fill to capacity without any consumer: peak must equal capacity and the
	// ninth enqueue must block (backpressure), proving no unbounded growth.
	for i := 0; i < 8; i++ {
		if err := q.Enqueue(i, ctx.Done()); err != nil {
			t.Fatal(err)
		}
	}
	if q.Peak() != 8 || q.Len() != 8 {
		t.Fatalf("peak=%d len=%d, want 8/8", q.Peak(), q.Len())
	}
	blocked := make(chan struct{})
	go func() {
		_ = q.Enqueue(9, ctx.Done())
		close(blocked)
	}()
	select {
	case <-blocked:
		t.Fatal("enqueue on full queue must block")
	case <-time.After(30 * time.Millisecond):
	}
	if q.BackpressureEvents() < 1 {
		t.Fatal("full queue must record a backpressure event")
	}
	if v, ok := q.Dequeue(ctx.Done()); !ok || v != 0 {
		t.Fatalf("dequeue = %d, ok=%v", v, ok)
	}
	select {
	case <-blocked:
	case <-time.After(time.Second):
		t.Fatal("producer did not resume after drain")
	}
}

func TestQueueCancelReleasesWaiters(t *testing.T) {
	q, _ := NewBoundedQueue[string](2)
	ctx, cancel := context.WithCancel(context.Background())
	_ = q.Enqueue("a", ctx.Done())
	_ = q.Enqueue("b", ctx.Done())
	errCh := make(chan error, 1)
	go func() { errCh <- q.Enqueue("c", ctx.Done()) }()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case err := <-errCh:
		if diag.CodeOf(err) != diag.EParam {
			t.Fatalf("canceled enqueue error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancel did not release blocked producer")
	}
	// Consumers unblock as well on cancel.
	if _, ok := q.Dequeue(ctx.Done()); ok {
		t.Fatal("dequeue after cancel must report not-ok")
	}
}

func TestQueueCapacityLimit(t *testing.T) {
	if _, err := NewBoundedQueue[int](0); diag.CodeOf(err) != diag.EParam {
		t.Fatalf("zero capacity must be E_PARAM, got %v", err)
	}
	big := diag.DefaultLimits().MaxQueueCapacity + 1
	if _, err := NewBoundedQueue[int](big); diag.CodeOf(err) != diag.EParam {
		t.Fatalf("over-limit capacity must be E_PARAM, got %v", err)
	}
}

func TestRunnerFullRunManifestAndMemory(t *testing.T) {
	dir := t.TempDir()
	writerDir = dir
	g := newTestGraph(t, []string{"v-600", "v-1200", "v-2400"})
	store := checkpoint.NewStore(dir)
	r, err := NewRunner(g, 8, FSWriter{Dir: dir}, store, dir,
		segment.Policy{TargetMS: 3000, MaxDeviationMS: 40})
	if err != nil {
		t.Fatal(err)
	}
	frames := demoFrames(timeline.FR30, 2700, 90, 0)
	feeds := []Input{
		{RepID: "v-600", Frames: frames},
		{RepID: "v-1200", Frames: frames},
		{RepID: "v-2400", Frames: frames},
	}
	stats, err := r.Run(context.Background(), feeds)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Segments != 30 || stats.Representations != 3 {
		t.Fatalf("stats = %+v", stats)
	}
	if stats.PeakQueue > 8 {
		t.Fatalf("peak queue %d exceeds configured capacity", stats.PeakQueue)
	}
	// Bounded by queue capacity, not the 2700-frame input: 3 reps * 8 * 64B.
	if stats.PeakMemoryBytes >= 33554432 {
		t.Fatalf("peak memory %d not bounded by queue config", stats.PeakMemoryBytes)
	}
	if len(stats.ManifestSHA) != 64 {
		t.Fatalf("manifest sha = %q", stats.ManifestSHA)
	}
}

func TestRunnerResumeMatchesFullRun(t *testing.T) {
	dir := t.TempDir()
	writerDir = dir
	g := newTestGraph(t, []string{"v-600", "v-1200"})
	frames := demoFrames(timeline.FR30, 1800, 90, 0)
	feeds := []Input{
		{RepID: "v-600", Frames: frames},
		{RepID: "v-1200", Frames: frames},
	}
	build := func() (*Runner, *checkpoint.Store) {
		st := checkpoint.NewStore(dir)
		r, err := NewRunner(g, 4, FSWriter{Dir: dir}, st, dir,
			segment.Policy{TargetMS: 3000, MaxDeviationMS: 40})
		if err != nil {
			t.Fatal(err)
		}
		return r, st
	}

	// Full reference run.
	r1, _ := build()
	full, err := r1.Run(context.Background(), feeds)
	if err != nil {
		t.Fatal(err)
	}

	// Wipe outputs but keep the checkpoint, then corrupt one segment file: the
	// resume run must detect it, redo from that point and hash identically.
	target := checkpoint.SegmentPath(dir, "v-600", 7)
	_ = writeFile(target, []byte("corrupted-bytes"))
	r2, _ := build()
	resumed, err := r2.Run(context.Background(), feeds)
	if err != nil {
		t.Fatalf("resume failed: %v", err)
	}
	if resumed.ManifestSHA != full.ManifestSHA {
		t.Fatalf("resume hash %s != full hash %s", resumed.ManifestSHA, full.ManifestSHA)
	}
	if resumed.RedoneSegments <= 0 {
		t.Fatal("resume must redo at least the corrupted segment")
	}
	if resumed.ResumeMS > 2000 {
		t.Fatalf("resume took %dms, budget 2000ms", resumed.ResumeMS)
	}
}

func TestRunnerDiskFailureThenRecover(t *testing.T) {
	dir := t.TempDir()
	writerDir = dir
	g := newTestGraph(t, []string{"v-600"})
	frames := demoFrames(timeline.FR30, 900, 90, 0)
	feeds := []Input{{RepID: "v-600", Frames: frames}}
	st := checkpoint.NewStore(dir)
	fw := &failingWriter{failAt: 5}
	r, err := NewRunner(g, 4, fw, st, dir,
		segment.Policy{TargetMS: 3000, MaxDeviationMS: 40})
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.Run(context.Background(), feeds)
	if diag.CodeOf(err) != diag.EOutput {
		t.Fatalf("disk failure must surface E_OUTPUT, got %v", err)
	}
	// A second run with healthy output resumes past the failed segment.
	r2, err := NewRunner(g, 4, FSWriter{Dir: dir}, checkpoint.NewStore(dir), dir,
		segment.Policy{TargetMS: 3000, MaxDeviationMS: 40})
	if err != nil {
		t.Fatal(err)
	}
	stats, err := r2.Run(context.Background(), feeds)
	if err != nil {
		t.Fatalf("recovery run failed: %v", err)
	}
	if stats.Segments != 10 {
		t.Fatalf("recovered segments = %d, want 10", stats.Segments)
	}
}

func TestRunnerCancellation(t *testing.T) {
	dir := t.TempDir()
	g := newTestGraph(t, []string{"v-600"})
	frames := demoFrames(timeline.FR30, 2700, 90, 0)
	ctx, cancel := context.WithCancel(context.Background())
	r, err := NewRunner(g, 2, FSWriter{Dir: dir}, checkpoint.NewStore(dir), dir,
		segment.Policy{TargetMS: 3000, MaxDeviationMS: 40})
	if err != nil {
		t.Fatal(err)
	}
	cancel() // already canceled: Run must return promptly, never hang.
	done := make(chan struct{})
	go func() { _, _ = r.Run(ctx, []Input{{RepID: "v-600", Frames: frames}}); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("canceled run did not return")
	}
}

func writeFile(path string, b []byte) error {
	return os.WriteFile(path, b, 0o644)
}
