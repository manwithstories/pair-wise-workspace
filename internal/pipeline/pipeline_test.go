package pipeline

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"mediacore/internal/media"
)

// memSource serves payload bytes with optional limits, so a test can model a
// slow medium, a truncated file and a source that never supplies the full
// requested window.
type memSource struct {
	data     []byte
	truncate int64
	maxChunk int64
}

func (m *memSource) ReadAt(p []byte, off int64) (int, error) {
	end := m.Size()
	if off >= end {
		return 0, io.EOF
	}
	if int64(len(p)) > end-off {
		p = p[:end-off]
	}
	if m.maxChunk > 0 && int64(len(p)) > m.maxChunk {
		p = p[:m.maxChunk]
	}
	n := copy(p, m.data[off:off+int64(len(p))])
	if off+int64(n) >= end {
		return n, io.EOF
	}
	return n, nil
}

func (m *memSource) Size() int64 {
	if m.truncate > 0 && m.truncate < int64(len(m.data)) {
		return m.truncate
	}
	return int64(len(m.data))
}
func (m *memSource) Name() string { return "mem" }

func makeTrack(count, size int) *media.Track {
	idx := make(media.SliceIndex, count)
	for i := range idx {
		idx[i] = media.Sample{
			Seq: uint32(i), DTS: media.RatFromInt(int64(i)), PTS: media.RatFromInt(int64(i)),
			Duration: media.RatFromInt(1), Offset: int64(i * size), Size: int64(size), Sync: true,
		}
	}
	return &media.Track{ID: media.TrackID{Container: "mp4", Value: 1}, Index: idx}
}

func fill(n int, v byte) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = v
	}
	return b
}

func TestQueueBoundsBytesAndDepth(t *testing.T) {
	src := &memSource{data: fill(40*1024, 0xab)}
	tr := makeTrack(40, 1024)
	q := NewQueue(src, Limits{MaxSamples: 4, MaxBytes: 8 << 10, ReadChunk: 256})
	ctx := context.Background()
	go func() {
		for i := 0; i < tr.SampleCount(); i++ {
			if err := q.Push(ctx, tr, i); err != nil {
				return
			}
		}
		q.Close()
	}()
	seen := 0
	for {
		err := q.Next(ctx, func(*Sample) error {
			seen++
			time.Sleep(time.Millisecond)
			return nil
		})
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("next: %v", err)
		}
	}
	if seen != 40 {
		t.Fatalf("seen=%d", seen)
	}
	st := q.Stats()
	if st.MaxQueueBytes > 8<<10 {
		t.Fatalf("peak %d exceeds 8 KiB budget", st.MaxQueueBytes)
	}
	if st.MaxQueueDepth > 4 {
		t.Fatalf("peak depth %d exceeds 4", st.MaxQueueDepth)
	}
}

func TestQueueCancellationPropagatesUpstream(t *testing.T) {
	src := &memSource{data: fill(64*1024, 0x11)}
	tr := makeTrack(64, 1024)
	q := NewQueue(src, Limits{MaxSamples: 2, MaxBytes: 4096, ReadChunk: 256})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		for i := 0; i < tr.SampleCount(); i++ {
			if err := q.Push(ctx, tr, i); err != nil {
				done <- err
				return
			}
		}
		q.Close()
		done <- nil
	}()
	if err := q.Next(ctx, func(*Sample) error { return nil }); err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, media.ErrCancelled) {
			t.Fatalf("class=%s err=%v", media.ClassOf(err), err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("producer did not observe cancellation")
	}
}

func TestQueueTruncationReportsFirstMissingByte(t *testing.T) {
	src := &memSource{data: fill(4096, 0x22), truncate: 3000, maxChunk: 100}
	tr := makeTrack(4, 1024)
	q := NewQueue(src, Limits{MaxSamples: 2, MaxBytes: 4096, ReadChunk: 64})
	err := q.Push(context.Background(), tr, 3)
	if !errors.Is(err, media.ErrTruncated) {
		t.Fatalf("class=%s err=%v", media.ClassOf(err), err)
	}
	if media.OffsetOf(err) != 3000 {
		t.Fatalf("offset=%d want 3000", media.OffsetOf(err))
	}
}

func TestQueueRejectsOversizedSample(t *testing.T) {
	src := &memSource{data: fill(10, 1)}
	tr := makeTrack(1, 1024)
	q := NewQueue(src, Limits{MaxSamples: 1, MaxBytes: 512})
	err := q.Push(context.Background(), tr, 0)
	if !errors.Is(err, media.ErrInvalidLength) {
		t.Fatalf("class=%s err=%v", media.ClassOf(err), err)
	}
}

func TestCheckpointRoundTripAndRollback(t *testing.T) {
	dir := t.TempDir()
	outPath := filepath.Join(dir, "out.bin")
	cpPath := filepath.Join(dir, "cp.json")
	out, err := os.OpenFile(outPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := out.Write(fill(16, 'a')); err != nil {
		t.Fatal(err)
	}
	m := NewManager(cpPath)
	m.Init("fp", "map", outPath, 4)
	if err := m.AppendSegment(out, 0, 16, 1, []TrackState{{Key: "mp4:1", OutputID: 1, Written: 1, SHA256: "aa"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := out.Write(fill(16, 'b')); err != nil {
		t.Fatal(err)
	}
	if err := m.AppendSegment(out, 16, 32, 2, []TrackState{{Key: "mp4:1", OutputID: 1, Written: 2, SHA256: "bb"}}); err != nil {
		t.Fatal(err)
	}

	plan, err := Load(cpPath, "fp", "map", outPath)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Offset != 32 || plan.Wave != 2 || plan.Written["mp4:1"] != 2 {
		t.Fatalf("plan=%+v", plan)
	}

	// Corrupting the last segment must roll back to the first.
	f, err := os.OpenFile(outPath, os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte{0xff}, 20); err != nil {
		t.Fatal(err)
	}
	f.Close()
	plan, err = Load(cpPath, "fp", "map", outPath)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Offset != 16 || plan.RolledBack != 1 {
		t.Fatalf("plan=%+v", plan)
	}
	info, _ := os.Stat(outPath)
	if info.Size() != 16 {
		t.Fatalf("output not rolled back: %d", info.Size())
	}
}

func TestCheckpointRejectsWrongInputMappingOrOutput(t *testing.T) {
	dir := t.TempDir()
	outPath := filepath.Join(dir, "out.bin")
	cpPath := filepath.Join(dir, "cp.json")
	if err := os.WriteFile(outPath, make([]byte, 8), 0o644); err != nil {
		t.Fatal(err)
	}
	m := NewManager(cpPath)
	m.Init("fp", "map", outPath, 1)
	if err := m.Complete(map[string]string{"mp4:1": "aa"}); err != nil {
		t.Fatal(err)
	}
	cases := []struct{ fp, sign, out string }{
		{"other", "map", outPath},
		{"fp", "other", outPath},
		{"fp", "map", filepath.Join(dir, "other.bin")},
	}
	for _, c := range cases {
		if _, err := Load(cpPath, c.fp, c.sign, c.out); !errors.Is(err, media.ErrCheckpointCorrupt) {
			t.Fatalf("case %+v: class=%s err=%v", c, media.ClassOf(err), err)
		}
	}
}

func TestCompletedCheckpointReadsBack(t *testing.T) {
	dir := t.TempDir()
	cpPath := filepath.Join(dir, "cp.json")
	m := NewManager(cpPath)
	m.Init("fp", "map", filepath.Join(dir, "out.mp4"), 12)
	if err := m.RecordFinal("mp4:1", "in", "out"); err != nil {
		t.Fatal(err)
	}
	if err := m.Complete(map[string]string{"mp4:1": "out"}); err != nil {
		t.Fatal(err)
	}
	cp, err := Read(cpPath)
	if err != nil {
		t.Fatal(err)
	}
	if !cp.Complete || cp.FinalSHA["mp4:1"] != "out" || cp.FinalInputSHA["mp4:1"] != "in" {
		t.Fatalf("cp=%+v", cp)
	}
}

// TestResumeVerificationIsFast asserts the documented bound: verifying a
// checkpoint and rolling back one corrupt segment must take well under two
// seconds.
func TestResumeVerificationIsFast(t *testing.T) {
	dir := t.TempDir()
	outPath := filepath.Join(dir, "out.bin")
	cpPath := filepath.Join(dir, "cp.json")
	out, err := os.OpenFile(outPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	m := NewManager(cpPath)
	m.Init("fp", "map", outPath, 100)
	// 100 segments of 4 KiB: enough to make verification meaningful.
	for i := 0; i < 100; i++ {
		start := int64(i * 4096)
		if _, err := out.Write(fill(4096, byte(i))); err != nil {
			t.Fatal(err)
		}
		if err := m.AppendSegment(out, start, start+4096, i+1, nil); err != nil {
			t.Fatal(err)
		}
	}
	out.Close()
	// Damage the newest segment so the rollback path is exercised.
	f, err := os.OpenFile(outPath, os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte{0xff}, 100*4096-10); err != nil {
		t.Fatal(err)
	}
	f.Close()

	begin := time.Now()
	plan, err := Load(cpPath, "fp", "map", outPath)
	if err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(begin)
	if elapsed > 2*time.Second {
		t.Fatalf("resume verification took %v, above the 2s bound", elapsed)
	}
	if plan.RolledBack != 1 || plan.Offset != 99*4096 {
		t.Fatalf("plan=%+v", plan)
	}
}
