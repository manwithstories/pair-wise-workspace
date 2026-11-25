package stream_test

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"mediacheck/internal/media"
	"mediacheck/internal/stream"
)

func TestQueueAppliesByteBackpressure(t *testing.T) {
	q := stream.NewQueue(stream.Config{MaxItems: 10, MaxBytes: 100})
	if err := q.Put(context.Background(), stream.Item{Offset: 0, Length: 60, Payload: make([]byte, 60)}); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		done <- q.Put(context.Background(), stream.Item{Offset: 60, Length: 60, Payload: make([]byte, 60)})
	}()
	select {
	case err := <-done:
		t.Fatalf("second put should block, got err=%v", err)
	case <-time.After(20 * time.Millisecond):
	}
	if q.ByteLen() != 60 {
		t.Fatalf("byte len=%d", q.ByteLen())
	}
	item, err := q.Get(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	q.Ack(item.EndOffset())
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if q.ProcessedPosition() != 60 {
		t.Fatalf("processed=%d", q.ProcessedPosition())
	}
}

func TestQueueRejectsOversizedItem(t *testing.T) {
	q := stream.NewQueue(stream.Config{MaxBytes: 16})
	err := q.Put(context.Background(), stream.Item{Offset: 4, Payload: make([]byte, 32)})
	var me *media.Error
	if !errors.As(err, &me) || me.Kind != media.KindResource {
		t.Fatalf("err=%v", err)
	}
}

func TestCancelReleasesBlockedPutAndGet(t *testing.T) {
	q := stream.NewQueue(stream.Config{MaxItems: 1, MaxBytes: 8})
	if err := q.Put(context.Background(), stream.Item{Payload: make([]byte, 8)}); err != nil {
		t.Fatal(err)
	}
	putCtx, cancelPut := context.WithCancel(context.Background())
	cancelPut()
	err := q.Put(putCtx, stream.Item{Payload: make([]byte, 8)})
	if code := media.ExitCode(err); code != 75 {
		t.Fatalf("put cancellation code=%d err=%v", code, err)
	}

	empty := stream.NewQueue(stream.Config{})
	getCtx, cancelGet := context.WithCancel(context.Background())
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancelGet()
	}()
	_, err = empty.Get(getCtx)
	if code := media.ExitCode(err); code != 75 {
		t.Fatalf("get cancellation code=%d err=%v", code, err)
	}
}

func TestCloseDrainsItemsThenEOF(t *testing.T) {
	q := stream.NewQueue(stream.Config{MaxItems: 2, MaxBytes: 100})
	_ = q.Put(context.Background(), stream.Item{Offset: 0, Length: 1, Payload: []byte("a")})
	q.Close()
	if _, err := q.Get(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := q.Get(context.Background()); !errors.Is(err, io.EOF) {
		t.Fatalf("expected EOF, got %v", err)
	}
}
