package txn

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"mediamerge/internal/demux"
	"mediamerge/internal/fixture"
	"mediamerge/internal/mapping"
	"mediamerge/internal/model"
	"mediamerge/internal/stream"
)

// TestContextCancelReportsBoundary cancels mid-run and asserts that the error
// carries the last complete sample boundary, so operators know exactly where
// the output stops being trustworthy.
func TestContextCancelReportsBoundary(t *testing.T) {
	dir, r, plan := prepareBig(t)
	defer r.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out, _ := Paths(dir)
	go func() {
		for i := 0; i < 2000; i++ {
			if info, err := os.Stat(out); err == nil && info.Size() > 512*1024 {
				cancel()
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Millisecond):
			}
		}
	}()
	_, err := Run(ctx, dir, r, plan, Options{Queue: 1, InterruptAfter: -1})
	if err == nil {
		t.Fatal("expected cancellation error")
	}
	var pe *model.Error
	if !errors.As(err, &pe) {
		t.Fatalf("error type %T: %v", err, err)
	}
	if pe.Code != model.CodeCanceled && pe.Code != model.CodeConsumerGone &&
		pe.Code != model.CodeStream {
		t.Fatalf("unexpected code %s (%v)", pe.Code, pe)
	}
	if pe.Boundary == nil && pe.Code == model.CodeCanceled {
		t.Fatalf("cancellation lost the last complete boundary: %v", pe)
	}
}

// TestConsumerExitClosesProducer verifies that a failing writer unblocks a
// producer blocked on a full queue instead of leaking the goroutine.
func TestConsumerExitClosesProducer(t *testing.T) {
	p := stream.New(context.Background(), 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 100; i++ {
			if err := p.Send(sampleOf(i)); err != nil {
				return
			}
		}
	}()
	if _, _, err := p.Recv(); err != nil {
		t.Fatal(err)
	}
	p.Fail(errors.New("writer failed"))
	select {
	case <-done:
	case <-context.Background().Done():
	}
	if p.Err() == nil {
		t.Fatal("pipe lost terminal error")
	}
}

func prepareBig(t *testing.T) (string, *demux.Reader, *mapping.Plan) {
	t.Helper()
	dir := t.TempDir()
	in := filepath.Join(dir, "in.mp4")
	if err := os.WriteFile(in, fixture.DefaultPlan(3, 400).Build(), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := demux.Open(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := mapping.Apply(r.Tracks, mapping.DefaultRules(r.Tracks))
	if err != nil {
		t.Fatal(err)
	}
	return dir, r, plan
}

func sampleOf(i int) model.Sample {
	return model.Sample{TrackIndex: 0, TrackID: 1, Index: i, Offset: int64(i), Data: make([]byte, 8)}
}

// TestMergeExplicitOutput checks that Merge writes to an arbitrary path.
func TestMergeExplicitOutput(t *testing.T) {
	dir, r, plan := prepare(t)
	defer r.Close()
	out := filepath.Join(dir, "explicit.mp4")
	res, err := Merge(context.Background(), dir, out, r, plan, Options{Queue: 2, InterruptAfter: -1})
	if err != nil {
		t.Fatal(err)
	}
	if !res.SampleMatch || res.OutputLen == 0 {
		t.Fatalf("res=%+v", res)
	}
	if _, err := os.Stat(out); err != nil {
		t.Fatal(err)
	}
}
