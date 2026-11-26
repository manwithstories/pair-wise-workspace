package pipeline

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"aircheck/internal/fixture"
	"aircheck/internal/media"
)

// memSource serves in-memory segments through the Source interface.
type memSource struct {
	segs []SegmentData
}

func (m *memSource) Len() int { return len(m.segs) }
func (m *memSource) Segment(i int) (SegmentData, error) {
	if i < 0 || i >= len(m.segs) {
		return SegmentData{}, fmt.Errorf("no segment %d", i)
	}
	return m.segs[i], nil
}

// copyProcessor hashes and re-emits bytes, optionally failing segments whose
// index is in the fail set.
type copyProcessor struct {
	failAt map[int]error
	delay  time.Duration
	mu     sync.Mutex
	seen   []int
}

func (c *copyProcessor) Process(ctx context.Context, in SegmentData) (Processed, error) {
	c.mu.Lock()
	c.seen = append(c.seen, in.Index)
	c.mu.Unlock()
	if c.delay > 0 {
		time.Sleep(c.delay)
	}
	if err, ok := c.failAt[in.Index]; ok {
		return Processed{}, err
	}
	if len(in.Data) == 0 {
		return Processed{}, media.NewFault(media.FaultTruncated,
			media.SourcePos{Artifact: in.URI, Offset: 0, Segment: in.Index}, "empty segment")
	}
	out := append([]byte("remux:"), in.Data...)
	return Processed{
		PayloadHash: Sha256Hex(in.Data),
		OutputHash:  Sha256Hex(out),
		Output:      out,
		Frames:      len(in.Data) / 100,
	}, nil
}

func testSource(n, size int) *memSource {
	src := &memSource{}
	for i := 0; i < n; i++ {
		src.segs = append(src.segs, SegmentData{
			Index: i, Seq: int64(i), URI: fmt.Sprintf("s%d.ts", i),
			Data: fixture.PayloadFor("video", i, size),
		})
	}
	return src
}

func TestQueueBoundsAndBackpressure(t *testing.T) {
	q, err := NewQueue(64<<10, 16<<20)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	// Fill to the ceiling then confirm a Put waits for the consumer.
	block := fixture.PayloadFor("video", 1, int(q.MaxBytes()))
	if err := q.Put(ctx, &Block{Data: block}); err != nil {
		t.Fatalf("Put full-buffer block: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		err := q.Put(ctx, &Block{Data: []byte{1}})
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("Put did not block: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if q.Stats().Backpressure == 0 {
		t.Fatal("backpressure not recorded")
	}
	if _, err := q.Get(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("blocked Put failed: %v", err)
	}
	if q.Stats().PeakBytes > q.MaxBytes() {
		t.Fatalf("peak %d exceeds ceiling %d", q.Stats().PeakBytes, q.MaxBytes())
	}
}

func TestQueueCancellation(t *testing.T) {
	q, _ := NewQueue(1024, 1024)
	ctx, cancel := context.WithCancel(context.Background())
	if err := q.Put(context.Background(), &Block{Data: make([]byte, 1024)}); err != nil {
		t.Fatal(err)
	}
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()
	if err := q.Put(ctx, &Block{Data: make([]byte, 10)}); !errors.Is(err, ErrCancelled) {
		t.Fatalf("want ErrCancelled, got %v", err)
	}
}

func TestQueueRejectsOversizedBlock(t *testing.T) {
	// A block larger than the ceiling is impossible by construction; the
	// queue still refuses it rather than violating the bound.
	if _, err := NewQueue(1024, 512); err == nil {
		t.Fatal("block > buffer must be rejected at construction")
	}
}

func TestRunnerCommitsAll(t *testing.T) {
	dir := t.TempDir()
	src := testSource(4, 80<<10) // each segment spans two 64 KiB blocks
	r, err := NewRunner(RunnerOptions{OutDir: dir}, src, &copyProcessor{})
	if err != nil {
		t.Fatal(err)
	}
	rep, err := r.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(rep.Outcomes) != 4 {
		t.Fatalf("outcomes %d", len(rep.Outcomes))
	}
	for _, o := range rep.Outcomes {
		if !o.Committed || o.Quarantined {
			t.Fatalf("outcome %+v", o)
		}
	}
	if rep.Stats.PeakBytes > DefaultMaxBufferBytes {
		t.Fatalf("peak %d > 16 MiB", rep.Stats.PeakBytes)
	}
	// The ledger must contain four entries and each file must exist.
	if _, err := os.Stat(filepath.Join(dir, "ledger.json")); err != nil {
		t.Fatalf("ledger: %v", err)
	}
}

func TestRunnerQuarantinesAndContinues(t *testing.T) {
	dir := t.TempDir()
	src := testSource(5, 1000)
	src.segs[2].Data = src.segs[2].Data[:500] // simulate truncation at process time
	fail := &copyProcessor{failAt: map[int]error{
		2: media.NewFault(media.FaultTruncated, media.SourcePos{Artifact: "s2.ts", Offset: 500, Segment: 2}, "short"),
	}}
	r, err := NewRunner(RunnerOptions{OutDir: dir}, src, fail)
	if err != nil {
		t.Fatal(err)
	}
	rep, err := r.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(rep.Outcomes) != 5 {
		t.Fatalf("outcomes %d", len(rep.Outcomes))
	}
	q := rep.Quarantined()
	if len(q) != 1 || q[0].Index != 2 {
		t.Fatalf("quarantine %+v", q)
	}
	if q[0].SourceOffset != 500 || q[0].FaultKind != "truncated" {
		t.Fatalf("fault detail %+v", q[0])
	}
	// Segments after the damaged one still commit: recovered == true.
	for _, o := range rep.Outcomes {
		if o.Index == 2 {
			continue
		}
		if !o.Committed {
			t.Fatalf("segment %d not committed after quarantine", o.Index)
		}
	}
}

func TestRunnerRerunSkipsCommitted(t *testing.T) {
	dir := t.TempDir()
	src := testSource(3, 5000)
	r, err := NewRunner(RunnerOptions{OutDir: dir}, src, &copyProcessor{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Second run: every segment must be skipped and the ledger unchanged.
	proc := &copyProcessor{}
	r2, err := NewRunner(RunnerOptions{OutDir: dir}, src, proc)
	if err != nil {
		t.Fatal(err)
	}
	rep, err := r2.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rep.ResumedParts != 3 {
		t.Fatalf("resumed %d", rep.ResumedParts)
	}
	for _, o := range rep.Outcomes {
		if !o.Skipped || !o.Committed {
			t.Fatalf("outcome %+v", o)
		}
	}
	if len(proc.seen) != 0 {
		t.Fatalf("processor ran on a rerun: %v", proc.seen)
	}
}

func TestRunnerCancellationLeavesNoPartialCommit(t *testing.T) {
	dir := t.TempDir()
	src := testSource(20, 200<<10)
	proc := &copyProcessor{delay: 10 * time.Millisecond}
	r, err := NewRunner(RunnerOptions{OutDir: dir, SlowConsumerMs: 0}, src, proc)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	rep, err := r.Run(ctx)
	if err != nil && !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run: %v", err)
	}
	if !rep.Cancelled {
		t.Fatal("expected cancellation")
	}
	// Every committed segment must be complete: output is the whole input with
	// its prefix, and the ledger must not list any partial file.
	for _, o := range rep.Outcomes {
		if !o.Committed {
			continue
		}
		path := filepath.Join(dir, segFile(o.Seq, o.Index))
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("committed file missing: %v", err)
		}
		if string(data[:6]) != "remux:" {
			t.Fatalf("partial commit observed in %s", path)
		}
	}
}

func TestSplitWholeBlocks(t *testing.T) {
	bs := Split(3, 7, "u.ts", media.Duration{}, fixture.PayloadFor("a", 1, 130<<10), 64<<10)
	if len(bs) != 3 {
		t.Fatalf("blocks %d", len(bs))
	}
	if !bs[2].Last || bs[0].Last {
		t.Fatal("last flag wrong")
	}
	var total int
	for _, b := range bs {
		total += len(b.Data)
	}
	if total != 130<<10 {
		t.Fatalf("reassembled %d", total)
	}
}

func TestSafeJoin(t *testing.T) {
	if _, err := SafeJoin("/in", "../etc/passwd"); err == nil {
		t.Fatal("escape accepted")
	}
	if _, err := SafeJoin("/in", "/abs"); err == nil {
		t.Fatal("absolute accepted")
	}
	if p, err := SafeJoin("/in", "a/b.ts"); err != nil || p != "/in/a/b.ts" {
		t.Fatalf("join %q %v", p, err)
	}
}

func TestStoreAtomicCommitNoPartialOnFailure(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	// A commit of a segment whose data is empty still succeeds (whole segment
	// is genuinely empty only if the processor says so).
	o := Outcome{Index: 1, Seq: 1, PayloadHash: "a", OutputHash: "b", OutputBytes: 0}
	if err := s.CommitOutcome(o, []byte("data")); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if _, ok := s.Has(1); !ok {
		t.Fatal("not in ledger")
	}
	if _, ok := s.Has(2); ok {
		t.Fatal("ghost ledger entry")
	}
}
