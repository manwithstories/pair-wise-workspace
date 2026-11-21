package pipeline

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"mediacore/internal/media"
)

// memSource serves a deterministic payload with optional truncation.
type memSource struct {
	data     []byte
	truncate int64
	limit    int64
}

func (m *memSource) ReadAt(p []byte, off int64) (int, error) {
	end := int64(len(m.data))
	if m.truncate > 0 && m.truncate < end {
		end = m.truncate
	}
	if off >= end {
		return 0, io.EOF
	}
	n := copy(p, m.data[off:end])
	if m.limit > 0 && int64(n) > m.limit {
		n = int(m.limit)
	}
	if n < len(p) {
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

func makeTrack(count int, size int) *media.Track {
	idx := make(media.SliceIndex, count)
	for i := range idx {
		idx[i] = media.Sample{
			Seq: uint32(i), DTS: media.RatFromInt(int64(i)), PTS: media.RatFromInt(int64(i)),
			Duration: media.RatFromInt(1), Offset: int64(i * size), Size: int64(size), Sync: true,
		}
	}
	return &media.Track{ID: media.TrackID{Container: "mp4", Value: 1}, Index: idx}
}

func TestQueueBoundsBytesAndBackpressures(t *testing.T) {
	payload := bytes.Repeat([]byte{0xab}, 40*1024)
	src := &memSource{data: payload, limit: 1000}
	tr := makeTrack(40, 1024)
	q := NewQueue(src, Limits{MaxSamples: 4, MaxBytes: 8 << 10, ReadChunk: 256})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
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
		err := q.Next(ctx, func(s *Sample) error {
			seen++
			time.Sleep(time.Millisecond)
			return nil
		})
		if err != nil {
			break
		}
	}
	if seen != 40 {
		t.Fatalf("seen=%d err=%v", seen, q.Err())
	}
	st := q.Stats()
	if st.MaxQueueBytes > 8<<10 {
		t.Fatalf("peak bytes %d exceeds limit", st.MaxQueueBytes)
	}
	if st.MaxQueueDepth > 4 {
		t.Fatalf("peak depth %d exceeds limit", st.MaxQueueDepth)
	}
}

func TestQueueCancellationReleases(t *testing.T) {
	payload := bytes.Repeat([]byte{0x11}, 32*1024)
	src := &memSource{data: payload, limit: 512}
	tr := makeTrack(32, 1024)
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

	// Consume one sample then cancel.
	if err := q.Next(ctx, func(s *Sample) error { return nil }); err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("producer should have been cancelled")
		}
		if media.ClassOf(err) != media.ClassCancelled {
			t.Fatalf("class=%s err=%v", media.ClassOf(err), err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("producer did not observe cancellation")
	}
}

func TestQueueTruncationReportsOffset(t *testing.T) {
	payload := bytes.Repeat([]byte{0x22}, 4096)
	src := &memSource{data: payload, truncate: 3000, limit: 100}
	tr := makeTrack(4, 1024)
	q := NewQueue(src, Limits{MaxSamples: 2, MaxBytes: 4096, ReadChunk: 64})
	err := q.Push(context.Background(), tr, 3)
	if err == nil {
		t.Fatal("expected truncation")
	}
	if media.ClassOf(err) != media.ClassTruncated {
		t.Fatalf("class=%s", media.ClassOf(err))
	}
	if media.OffsetOf(err) != 3000 {
		t.Fatalf("offset=%d", media.OffsetOf(err))
	}
}

func TestQueueRejectsOversizedSample(t *testing.T) {
	src := &memSource{data: make([]byte, 10)}
	tr := makeTrack(1, 1024)
	q := NewQueue(src, Limits{MaxSamples: 1, MaxBytes: 512})
	err := q.Push(context.Background(), tr, 0)
	if media.ClassOf(err) != media.ClassInvalidLength {
		t.Fatalf("class=%s err=%v", media.ClassOf(err), err)
	}
	if !errors.Is(err, media.ErrInvalidLength) {
		t.Fatal("errors.Is should match the class")
	}
}
