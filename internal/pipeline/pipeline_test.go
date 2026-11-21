package pipeline

import (
	"bytes"
	"context"
	"crypto/rand"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func randBytes(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

func TestRunDeterministic(t *testing.T) {
	data := randBytes(t, 3*4096+100)
	cfg := Config{QueueCap: 4, ChunkSize: 4096}
	r1, err := Run(context.Background(), bytes.NewReader(data), cfg, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	r2, err := Run(context.Background(), bytes.NewReader(data), cfg, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(r1.Segments) != 4 {
		t.Fatalf("want 4 segments, got %d", len(r1.Segments))
	}
	if !SegmentHashesEqual(r1.Segments, r2.Segments) {
		t.Error("repeat run produced different boundaries/hashes")
	}
	for _, s := range r1.Segments {
		if len(s.SHA256) != 64 {
			t.Errorf("bad sha256 %q", s.SHA256)
		}
	}
	if r1.PeakBufferBytes > int64(cfg.QueueCap)*cfg.ChunkSize {
		t.Errorf("peak %d exceeds queue*chunk", r1.PeakBufferBytes)
	}
}

func TestSlowConsumerBackpressureAndCancel(t *testing.T) {
	data := randBytes(t, 64*4096)
	cfg := Config{QueueCap: 2, ChunkSize: 4096}
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	var once sync.Once
	slow := func(s *SegmentHash) error {
		once.Do(func() { close(started) })
		time.Sleep(500 * time.Millisecond) // downstream stalls
		return nil
	}
	go func() {
		<-started
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	done := make(chan struct{})
	go func() {
		defer close(done)
		res, err := Run(ctx, bytes.NewReader(data), cfg, "", slow)
		if err != nil {
			t.Errorf("cancel should not be a hard error: %v", err)
		}
		if res == nil || len(res.Segments) >= 16 {
			t.Errorf("run should stop early after cancel: %+v", res)
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("pipeline did not shut down after cancel")
	}
}

func TestQueueCapValidation(t *testing.T) {
	for _, cap := range []int{0, 65, -1} {
		if _, err := Run(context.Background(), bytes.NewReader(nil),
			Config{QueueCap: cap, ChunkSize: 1}, "", nil); err == nil {
			t.Errorf("queue cap %d should be rejected", cap)
		}
	}
}

func TestCheckpointResume(t *testing.T) {
	data := randBytes(t, 5*2048+7)
	cfg := Config{QueueCap: 3, ChunkSize: 2048}
	cp := filepath.Join(t.TempDir(), "run.state")

	// Full run without checkpoint for reference.
	ref, err := Run(context.Background(), bytes.NewReader(data), cfg, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	// Interrupted run: cancel once the checkpoint shows a committed
	// prefix (simulating a crash mid-batch).
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	gate := func(s *SegmentHash) error {
		time.Sleep(20 * time.Millisecond) // slow align so commits trickle
		return nil
	}
	go func() {
		for {
			b, err := os.ReadFile(cp)
			if err == nil && strings.Contains(string(b), `"index": 1`) {
				cancel()
				return
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()
	part, err := Run(ctx, bytes.NewReader(data), cfg, cp, gate)
	if err != nil {
		t.Fatal(err)
	}
	if len(part.Segments) == 0 || len(part.Segments) >= len(ref.Segments) {
		t.Fatalf("interrupted run should commit a prefix: %d", len(part.Segments))
	}
	// Resume: must verify committed prefix and finish with identical hashes.
	fin, err := Run(context.Background(), bytes.NewReader(data), cfg, cp, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !fin.Resumed {
		t.Error("second run should resume from checkpoint")
	}
	if !fin.ResumeMatches {
		t.Error("resumed run hashes differ from first processing")
	}
	if !SegmentHashesEqual(fin.Segments, ref.Segments) {
		t.Error("resumed segments differ from uninterrupted run")
	}
	// Third run over a done checkpoint: still identical.
	again, err := Run(context.Background(), bytes.NewReader(data), cfg, cp, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !again.ResumeMatches || !SegmentHashesEqual(again.Segments, ref.Segments) {
		t.Error("re-run over completed checkpoint diverged")
	}
}

func TestCheckpointDetectsChangedInput(t *testing.T) {
	cfg := Config{QueueCap: 2, ChunkSize: 1024}
	cp := filepath.Join(t.TempDir(), "run.state")
	if _, err := Run(context.Background(), bytes.NewReader(randBytes(t, 3000)), cfg, cp, nil); err != nil {
		t.Fatal(err)
	}
	// Different input, same checkpoint: must fail verification.
	if _, err := Run(context.Background(), bytes.NewReader(randBytes(t, 3000)), cfg, cp, nil); err == nil {
		t.Fatal("changed input must be rejected")
	}
}

func TestCheckpointWriteFailureClosesUpstream(t *testing.T) {
	data := randBytes(t, 8*1024)
	cfg := Config{QueueCap: 2, ChunkSize: 1024}
	bad := filepath.Join(t.TempDir(), "no-such-dir", "run.state")
	done := make(chan error, 1)
	go func() {
		_, err := Run(context.Background(), bytes.NewReader(data), cfg, bad, nil)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Error("expected checkpoint write failure")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("pipeline hung on checkpoint write failure")
	}
}
