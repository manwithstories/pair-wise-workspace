package worker

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"avpipe/internal/media"
)

func makePkt(seq int64) media.Packet {
	return media.Packet{TrackID: "v0", Seq: seq, Arrival: seq, PTS: seq * 10, DTS: seq * 10,
		TimeBase: media.MustRat(1, 30)}
}

func TestPoolPreservesSubmissionOrder(t *testing.T) {
	p, err := New(context.Background(), Config{Workers: 4, Queue: 8, Cache: 8},
		func(ctx context.Context, p media.Packet) (media.Packet, error) {
			// Reverse the completion order deliberately.
			time.Sleep(time.Duration((10-p.Arrival)%5) * time.Millisecond)
			return p, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	const n = 50
	go func() {
		for i := int64(0); i < n; i++ {
			if err := p.Submit(makePkt(i)); err != nil {
				return
			}
		}
		p.Close()
	}()
	var got []int64
	for pkt := range p.Results() {
		got = append(got, pkt.Arrival)
	}
	if len(got) != n {
		t.Fatalf("got %d results", len(got))
	}
	for i, v := range got {
		if v != int64(i) {
			t.Fatalf("position %d = %d, order broken: %v", i, v, got)
		}
	}
	st := p.Stats()
	if st.Processed != n {
		t.Fatalf("processed = %d", st.Processed)
	}
	if st.CachePeak > 8 {
		t.Fatalf("cache peak %d exceeds limit 8", st.CachePeak)
	}
	if st.QueuePeak > 8 {
		t.Fatalf("queue peak %d exceeds limit 8", st.QueuePeak)
	}
}

func TestPoolBoundsCache(t *testing.T) {
	var blocked int32
	p, err := New(context.Background(), Config{Workers: 2, Queue: 4, Cache: 4},
		func(ctx context.Context, p media.Packet) (media.Packet, error) {
			atomic.AddInt32(&blocked, 1)
			return p, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	// Never drain Results: Submitting more than Queue+Cache must block, which
	// proves the bounds are enforced rather than silently growing.
	done := make(chan struct{})
	go func() {
		for i := int64(0); i < 100; i++ {
			if err := p.Submit(makePkt(i)); err != nil {
				close(done)
				return
			}
		}
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("submission completed without a consumer; bounds not enforced")
	case <-time.After(150 * time.Millisecond):
	}
	p.Cancel()
	// Drain so goroutines can exit.
	for range p.Results() {
	}
	p.Close()
	if st := p.Stats(); st.CachePeak > 4 {
		t.Fatalf("cache peak %d exceeds bound 4", st.CachePeak)
	}
}

func TestPoolCancelStopsPromptly(t *testing.T) {
	p, err := New(context.Background(), Config{Workers: 3, Queue: 4, Cache: 4},
		func(ctx context.Context, p media.Packet) (media.Packet, error) {
			select {
			case <-ctx.Done():
				return media.Packet{}, ctx.Err()
			case <-time.After(10 * time.Second):
				return p, nil
			}
		})
	if err != nil {
		t.Fatal(err)
	}
	for i := int64(0); i < 4; i++ {
		p.Submit(makePkt(i)) //nolint:errcheck
	}
	start := time.Now()
	p.Cancel()
	p.Close() // must return quickly once canceled, not after 10s
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("cancel took %v", elapsed)
	}
	// Drain any buffered results.
	for range p.Results() {
	}
}

func TestPoolPropagatesError(t *testing.T) {
	sentinel := media.Errorf(media.KindCorrupt, media.Origin{File: "x"}, "test", "boom")
	p, err := New(context.Background(), Config{Workers: 2, Queue: 4, Cache: 4},
		func(ctx context.Context, p media.Packet) (media.Packet, error) {
			if p.Arrival == 3 {
				return media.Packet{}, sentinel
			}
			return p, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for i := int64(0); i < 10; i++ {
			if err := p.Submit(makePkt(i)); err != nil {
				break
			}
		}
		p.Close()
	}()
	for range p.Results() {
	}
	if !errors.Is(p.Err(), sentinel) && media.KindOf(p.Err()) != media.KindCorrupt {
		t.Fatalf("err = %v", p.Err())
	}
}

func TestConfigValidation(t *testing.T) {
	if _, err := (Config{Workers: -1}).Validate(); err != nil {
		t.Fatalf("negative workers should clamp: %v", err)
	}
	if _, err := (Config{Queue: 8, Cache: 4}).Validate(); err == nil {
		t.Fatal("cache < queue must be rejected")
	}
	if _, err := (Config{Workers: 2000}).Validate(); err == nil {
		t.Fatal("excessive workers must be rejected")
	}
}
