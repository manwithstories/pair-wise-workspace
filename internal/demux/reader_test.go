package demux

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"mediamerge/internal/fixture"
	"mediamerge/internal/model"
)

func writeFixture(t *testing.T, n, samples int) (string, fixture.Plan) {
	t.Helper()
	plan := fixture.DefaultPlan(n, samples)
	dir := t.TempDir()
	path := filepath.Join(dir, "input.mp4")
	if err := os.WriteFile(path, plan.Build(), 0o644); err != nil {
		t.Fatal(err)
	}
	return path, plan
}

func TestReadFixture(t *testing.T) {
	path, plan := writeFixture(t, 3, 12)
	r, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer r.Close()
	if len(r.Tracks) != 3 {
		t.Fatalf("tracks=%d", len(r.Tracks))
	}
	total := 0
	for _, tr := range r.Tracks {
		if tr.SampleCount != 12 || len(tr.Samples) != 12 {
			t.Fatalf("track %d samples=%d/%d", tr.InputID, tr.SampleCount, len(tr.Samples))
		}
		for i := 1; i < len(tr.Samples); i++ {
			if tr.Samples[i].DTS <= tr.Samples[i-1].DTS {
				t.Fatalf("track %d DTS not increasing at %d", tr.InputID, i)
			}
		}
		// Every sample payload must start with its track id marker.
		for _, s := range tr.Samples {
			data, err := r.ReadSample(s)
			if err != nil {
				t.Fatalf("read sample: %v", err)
			}
			if int(data[0]) != int(tr.InputID) {
				t.Fatalf("track %d sample %d payload marker %d", tr.InputID, s.Index, data[0])
			}
		}
		total += len(tr.Samples)
	}
	if total != 36 {
		t.Fatalf("total samples=%d", total)
	}
	if r.MovieTimescale != 1000 {
		t.Fatalf("movie timescale=%d", r.MovieTimescale)
	}
	if len(r.Tracks[0].SampleEntry) == 0 {
		t.Fatal("missing sample entry")
	}
	_ = plan
}

func TestReadTruncatedFile(t *testing.T) {
	path, _ := writeFixture(t, 2, 8)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	cut := len(data) - 200
	if err := os.WriteFile(path, data[:cut], 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = Read(context.Background(), path)
	if err == nil {
		t.Fatal("expected truncation error")
	}
	pe, ok := err.(*model.Error)
	if !ok {
		t.Fatalf("unexpected error type %T", err)
	}
	if pe.Stage != model.StageDemux || pe.Code != model.CodeTruncated {
		t.Fatalf("error=%+v", pe)
	}
	if pe.Offset != int64(cut) && pe.Offset != int64(len(data)) {
		t.Fatalf("offset=%d want %d", pe.Offset, cut)
	}
}
