package stream

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"archive/internal/media"
)

func TestBackpressureAndPeak(t *testing.T) {
	p := NewPipe(100, 4)
	ctx := context.Background()
	// Fill the pipe: 4 chunks x 25 bytes = 100 bytes, at the limit.
	for i := 0; i < 4; i++ {
		if err := p.Send(ctx, make([]byte, 25)); err != nil {
			t.Fatal(err)
		}
	}
	// Next send must block until a recv frees space.
	done := make(chan error, 1)
	go func() { done <- p.Send(ctx, make([]byte, 25)) }()
	select {
	case <-done:
		t.Fatal("send did not block under backpressure")
	case <-time.After(50 * time.Millisecond):
	}
	if _, err := p.Recv(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("send still blocked after recv")
	}
	if p.Peak() != 100 {
		t.Fatalf("peak=%d want 100", p.Peak())
	}
}

func TestCleanCloseEOF(t *testing.T) {
	p := NewPipe(1024, 8)
	ctx := context.Background()
	p.Send(ctx, []byte("x"))
	p.Close(nil)
	if _, err := p.Recv(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Recv(ctx); err != io.EOF {
		t.Fatalf("want EOF, got %v", err)
	}
}

func TestWriteFailurePropagates(t *testing.T) {
	p := NewPipe(1024, 8)
	ctx := context.Background()
	boom := media.New(media.KindInternal, "index.commit", "v1", 5, "disk full", nil)
	p.Send(ctx, []byte("x"))
	p.Close(boom)
	p.Recv(ctx) // drain
	if _, err := p.Recv(ctx); !errors.Is(err, boom) {
		t.Fatalf("terminal error not propagated: %v", err)
	}
}

func TestDownstreamAbortReleasesProducer(t *testing.T) {
	p := NewPipe(10, 1)
	ctx := context.Background()
	p.Send(ctx, make([]byte, 10)) // full
	done := make(chan error, 1)
	go func() { done <- p.Send(ctx, make([]byte, 10)) }()
	time.Sleep(20 * time.Millisecond)
	p.Abort(errors.New("consumer crashed"))
	select {
	case err := <-done:
		me, ok := err.(*media.Error)
		if !ok || me.Kind != media.KindCanceled {
			t.Fatalf("want locatable canceled error, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("producer not released by abort")
	}
	if _, err := p.Recv(ctx); err == nil {
		t.Fatal("recv after abort must fail")
	}
}

func TestCancelReleasesProducer(t *testing.T) {
	p := NewPipe(10, 1)
	ctx, cancel := context.WithCancel(context.Background())
	p.Send(ctx, make([]byte, 10))
	done := make(chan error, 1)
	go func() { done <- p.Send(ctx, make([]byte, 10)) }()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("canceled send returned nil")
		}
	case <-time.After(time.Second):
		t.Fatal("producer not released by cancel")
	}
}
