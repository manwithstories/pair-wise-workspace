package stream

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"relay/internal/fixture"
	"relay/internal/ingest"
	"relay/internal/media"
	"relay/internal/timeline"
)

func TestBoundedQueueBackPressure(t *testing.T) {
	q := NewBoundedQueue(100)
	ctx := context.Background()
	// First item fits; second exceeds remaining budget and must block.
	if err := q.Enqueue(ctx, Item{Key: "a", Weight: 60}); err != nil {
		t.Fatal(err)
	}
	blocked := make(chan struct{})
	go func() {
		_ = q.Enqueue(ctx, Item{Key: "b", Weight: 60})
		close(blocked)
	}()
	select {
	case <-blocked:
		t.Fatal("enqueue must block when budget is exhausted")
	case <-time.After(30 * time.Millisecond):
	}
	it, ok, err := q.Dequeue(ctx)
	if err != nil || !ok || it.Key != "a" {
		t.Fatalf("unexpected dequeue: %+v %v %v", it, ok, err)
	}
	select {
	case <-blocked:
	case <-time.After(time.Second):
		t.Fatal("producer did not unblock after consumer drained")
	}
	if q.Peak() > 100 {
		t.Fatalf("peak %d exceeded budget 100", q.Peak())
	}
}

func TestOversizedItemRejected(t *testing.T) {
	q := NewBoundedQueue(10)
	err := q.Enqueue(context.Background(), Item{Key: "big", Weight: 11})
	var f *media.Fault
	if err == nil || !asFault(err, &f) || f.Code != media.FaultQueueLimit {
		t.Fatalf("expected queue limit fault, got %v", err)
	}
}

func asFault(err error, target **media.Fault) bool {
	f, ok := err.(*media.Fault)
	if ok {
		*target = f
	}
	return ok
}

// memSink records writes and returns deterministic hashes.
type memSink struct{ calls int }

func (m *memSink) Write(rf *timeline.RepairedFragment, outDir string) (int64, string, error) {
	m.calls++
	return int64(len(rf.Raw)), HashBytes(rf.Raw), nil
}

func buildReports(t *testing.T) []*timeline.TrackReport {
	t.Helper()
	sc := fixture.BuildAllScenario()
	var frags []*ingest.Fragment
	for _, in := range sc.Files {
		if in.Truncated {
			continue
		}
		f, err := ingest.ParseFragment(in.Name, 0, in.Bytes)
		if err != nil {
			t.Fatal(err)
		}
		frags = append(frags, f)
	}
	byTrack := map[uint32][]*ingest.Fragment{}
	for _, f := range frags {
		byTrack[f.TrackID] = append(byTrack[f.TrackID], f)
	}
	var reports []*timeline.TrackReport
	for _, id := range []uint32{101, 201, 301} {
		reports = append(reports, timeline.RepairTrack(id,
			media.Clock{TrackID: id, Timescale: timescaleOf(id)}, byTrack[id]))
	}
	return reports
}

func timescaleOf(id uint32) uint32 {
	if id == 101 {
		return 90000
	}
	return 48000
}

func TestCheckpointResumeSkipsCommitted(t *testing.T) {
	dir := t.TempDir()
	reports := buildReports(t)
	sink := &memSink{}

	first, err := New(Config{OutDir: dir, QueueBytes: 1 << 20, InterruptAfter: 9}, sink)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	c1, err := first.Run(ctx, reports)
	if err != ErrInterrupted {
		t.Fatalf("expected interrupt, got %v", err)
	}
	if c1.Committed != 9 {
		t.Fatalf("expected 9 commits, got %d", c1.Committed)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	// Resume: the same 9 fragments must be skipped, remaining ones written.
	sink2 := &memSink{}
	second, err := New(Config{OutDir: dir, QueueBytes: 1 << 20}, sink2)
	if err != nil {
		t.Fatal(err)
	}
	c2, err := second.Run(ctx, reports)
	if err != nil {
		t.Fatalf("resume run failed: %v", err)
	}
	totalAccepted := 0
	for _, r := range reports {
		totalAccepted += len(r.Accepted)
	}
	if c2.Skipped != 9 {
		t.Fatalf("expected 9 skipped, got %d", c2.Skipped)
	}
	if c2.Accepted != totalAccepted-9 {
		t.Fatalf("expected %d new commits, got %d", totalAccepted-9, c2.Accepted)
	}
	if len(second.Checkpoint().Entries()) != totalAccepted {
		t.Fatal("checkpoint must hold every accepted fragment after resume")
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestCheckpointPersistsAcrossHandles(t *testing.T) {
	dir := t.TempDir()
	cp, err := OpenCheckpoint(filepath.Join(dir))
	if err != nil {
		t.Fatal(err)
	}
	e := CheckpointEntry{Key: "t1/f1", TrackID: 1, Sequence: 1,
		SourceHash: "aa", OutputHash: "bb", OutputSize: 12}
	if err := cp.Commit(e); err != nil {
		t.Fatal(err)
	}
	if err := cp.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenCheckpoint(filepath.Join(dir))
	if err != nil {
		t.Fatal(err)
	}
	got, ok := reopened.Committed("t1/f1")
	if !ok || got.OutputHash != "bb" {
		t.Fatalf("checkpoint did not persist: %+v", got)
	}
	_ = reopened.Close()
}
