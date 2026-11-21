package txn

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"mediamerge/internal/checksum"
	"mediamerge/internal/demux"
	"mediamerge/internal/fixture"
	"mediamerge/internal/mapping"
	"mediamerge/internal/model"
	"mediamerge/internal/mux"
)

func prepare(t *testing.T) (string, *demux.Reader, *mapping.Plan) {
	t.Helper()
	dir := t.TempDir()
	in := filepath.Join(dir, "in.mp4")
	if err := os.WriteFile(in, fixture.DefaultPlan(3, 12).Build(), 0o644); err != nil {
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

func TestRunAndRecoverIdempotent(t *testing.T) {
	dir, r, plan := prepare(t)
	defer r.Close()
	work := filepath.Join(dir, "work")
	os.MkdirAll(work, 0o755)
	if err := copyInput(work, r.Path); err != nil {
		t.Fatal(err)
	}
	// Interrupt after stage 2 (track 2).
	res, err := Run(context.Background(), work, r, plan, Options{Queue: 2, InterruptAfter: 2, CrashBytes: -1})
	if !errors.Is(err, ErrInterrupted) {
		t.Fatalf("expected interruption, got %v", err)
	}
	if res.Samples != 24 {
		t.Fatalf("samples after interrupt=%d", res.Samples)
	}
	st1, err := Recover(work, r.InputSHA, r.Size, plan)
	if err != nil {
		t.Fatalf("recover 1: %v", err)
	}
	if st1.CommittedStage != 2 {
		t.Fatalf("committed stage=%d", st1.CommittedStage)
	}
	st2, err := Recover(work, r.InputSHA, r.Size, plan)
	if err != nil {
		t.Fatalf("recover 2: %v", err)
	}
	if st2.CommittedStage != st1.CommittedStage || st2.CommittedLen != st1.CommittedLen || st2.RollbackBytes != 0 {
		t.Fatalf("second recovery differs: %+v vs %+v", st2, st1)
	}
	// Resume from the committed boundary and finish.
	w2, lay, rolled, err := mux.OpenAt(filepath.Join(work, "output.mp4"), plan, st1.CommittedLen)
	if err != nil {
		t.Fatalf("open at boundary: %v", err)
	}
	if rolled != 0 {
		t.Fatalf("rolled back %d after recovery", rolled)
	}
	_ = lay
	for ti := 0; ti < len(plan.Tracks); ti++ {
		tr := plan.Tracks[ti]
		for si, ref := range tr.Samples {
			if w2.NextSample(ti) > si {
				continue
			}
			data, err := r.ReadSample(ref)
			if err != nil {
				t.Fatal(err)
			}
			if err := w2.WriteSample(ti, model.Sample{TrackID: tr.TargetID, Index: si, Data: data}); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := w2.Finalize(); err != nil {
		t.Fatal(err)
	}
	out, _ := Paths(work)
	sha, _, err := checksum.File(out)
	if err != nil {
		t.Fatal(err)
	}
	// Compare with a clean, uninterrupted run.
	clean := filepath.Join(dir, "clean")
	os.MkdirAll(clean, 0o755)
	cleanRes, err := Run(context.Background(), clean, r, plan, Options{Queue: 4})
	if err != nil {
		t.Fatalf("clean run: %v", err)
	}
	if sha != cleanRes.OutputSHA {
		t.Fatalf("resumed output %s != clean %s", sha, cleanRes.OutputSHA)
	}
	if !cleanRes.SampleMatch {
		t.Fatal("clean run sample mismatch")
	}
}
