package mux

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"mediamerge/internal/demux"
	"mediamerge/internal/fixture"
	"mediamerge/internal/mapping"
	"mediamerge/internal/model"
)

func setup(t *testing.T) (string, *demux.Reader, *mapping.Plan) {
	t.Helper()
	dir := t.TempDir()
	in := filepath.Join(dir, "in.mp4")
	if err := os.WriteFile(in, fixture.DefaultPlan(3, 20).Build(), 0o644); err != nil {
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

func TestWriterRoundTrip(t *testing.T) {
	dir, r, plan := setup(t)
	defer r.Close()
	out := filepath.Join(dir, "out.mp4")
	w, lay, err := New(out, plan)
	if err != nil {
		t.Fatal(err)
	}
	for ti, tr := range plan.Tracks {
		for _, ref := range tr.Samples {
			data, err := r.ReadSample(ref)
			if err != nil {
				t.Fatal(err)
			}
			if err := w.WriteSample(ti, model.Sample{TrackID: tr.TargetID, Index: ref.Index, Data: data}); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := w.Finalize(); err != nil {
		t.Fatal(err)
	}
	w.Close()
	if got, _ := os.Stat(out); got.Size() != lay.MovieEnd+int64(0) && got.Size() < lay.MovieEnd {
		t.Fatalf("output size %d < movie end %d", got.Size(), lay.MovieEnd)
	}
	rr, err := demux.Open(context.Background(), out)
	if err != nil {
		t.Fatalf("re-read output: %v", err)
	}
	defer rr.Close()
	if len(rr.Tracks) != len(plan.Tracks) {
		t.Fatalf("tracks=%d", len(rr.Tracks))
	}
	for ti, want := range plan.Tracks {
		got := rr.Tracks[ti]
		if got.SampleCount != want.SampleCount {
			t.Fatalf("track %d samples=%d want %d", ti, got.SampleCount, want.SampleCount)
		}
		for si, ref := range want.Samples {
			a, err := r.ReadSample(ref)
			if err != nil {
				t.Fatal(err)
			}
			b, err := rr.ReadSample(got.Samples[si])
			if err != nil {
				t.Fatal(err)
			}
			if string(a) != string(b) {
				t.Fatalf("track %d sample %d payload mismatch", ti, si)
			}
			if got.Samples[si].DTS != ref.DTS || got.Samples[si].Duration != ref.Duration {
				t.Fatalf("track %d sample %d timing mismatch", ti, si)
			}
		}
	}
}
