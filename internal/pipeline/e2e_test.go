package pipeline

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"relaycheck/internal/fixture"
	"relaycheck/internal/provenance"
	"relaycheck/internal/recovery"
)

func e2eFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	_, err := fixture.Generate(fixture.Options{
		Out: dir, Seed: 17, Segments: 96,
		Faults: []string{"gap", "rollback", "duplicate", "truncate", "loss", "map-conflict"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestEndToEnd(t *testing.T) {
	dir := e2eFixture(t)
	out := filepath.Join(dir, "out")
	idx := filepath.Join(dir, "idx")

	// clean stream: everything accepted, metrics at their bounds
	rep, err := Run(Config{Input: filepath.Join(dir, "clean"), Output: out, IndexPath: idx, Chunk: 16})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Accepted != 96 || rep.SyncMaxUs != 0 || rep.PayloadHashMatch != 1 || rep.Quarantined != 0 {
		t.Fatalf("clean report: %+v", rep)
	}
	if _, err := Verify(VerifyConfig{Input: filepath.Join(dir, "clean"), Output: out, IndexPath: idx}); err != nil {
		t.Fatal(err)
	}

	// faulty stream: stop after 2 chunks, then resume
	fout := filepath.Join(dir, "recovered")
	fidx := filepath.Join(dir, "recovery.idx")
	fcfg := Config{Input: filepath.Join(dir, "faulty"), Output: fout, IndexPath: fidx, Chunk: 16}
	_, err = Run(Config{Input: fcfg.Input, Output: fout, IndexPath: fidx, Chunk: 16, StopAfter: 2})
	if !errors.Is(err, ErrInterrupted) {
		t.Fatalf("expected ErrInterrupted, got %v", err)
	}
	x, _ := recovery.Open(fidx)
	if len(x.Chunks) != 2 || x.Done != nil {
		t.Fatalf("after stop: chunks=%d done=%v", len(x.Chunks), x.Done)
	}
	x.Close()
	rep, err = Run(fcfg)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Accepted+rep.Quarantined+rep.Lost != 96 {
		t.Fatalf("coverage: %+v", rep)
	}
	if rep.Duplicates != 1 || rep.Gaps != 1 || rep.Quarantined != 2 || rep.Lost != 1 {
		t.Fatalf("fault accounting: %+v", rep)
	}
	if rep.ResumedChunks != 4 {
		t.Fatalf("resumed_chunks=%d", rep.ResumedChunks)
	}
	if rep.SyncMaxUs > 1000 || rep.LossRecovery < 0.95 || rep.PayloadHashMatch != 1 {
		t.Fatalf("metrics out of bounds: %+v", rep)
	}
	vrep, err := Verify(VerifyConfig{Input: fcfg.Input, Output: fout, IndexPath: fidx})
	if err != nil {
		t.Fatal(err)
	}
	if vrep.ResultsDigest != rep.ResultsDigest {
		t.Fatal("verify digest mismatch")
	}

	// idempotent rerun: same digest, no new index lines
	before, _ := os.ReadFile(fidx)
	rep2, err := Run(fcfg)
	if err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(fidx)
	if rep2.ResultsDigest != rep.ResultsDigest || len(before) != len(after) {
		t.Fatal("rerun changed results or index")
	}
	// Re-issuing the planned stop against the finished index must still
	// report ErrInterrupted (exit 3) without appending to the index, so the
	// scripted "stop; test $? -eq 3; resume" block is re-runnable.
	stopCfg := fcfg
	stopCfg.StopAfter = 2
	if _, err := Run(stopCfg); !errors.Is(err, ErrInterrupted) {
		t.Fatalf("stop-after on a done run: expected ErrInterrupted, got %v", err)
	}
	again, _ := os.ReadFile(fidx)
	if len(again) != len(after) {
		t.Fatal("stop-after on done run modified the index")
	}
	// and the following resume still completes with the identical digest
	if _, err := Run(fcfg); err != nil {
		t.Fatal(err)
	}

	// sample-level provenance: accepted tracks carry one source digest and
	// offset per output sample, matching the declared sample count.
	pt, err := provenance.Open(filepath.Join(fout, "provenance"))
	if err != nil {
		t.Fatal(err)
	}
	all, err := pt.Load()
	if err != nil {
		t.Fatal(err)
	}
	foundRefs := false
	for _, st := range all {
		if st.Decision != provenance.Accepted {
			continue
		}
		for _, tt := range st.Tracks {
			if len(tt.Refs) == 0 {
				continue
			}
			foundRefs = true
			if len(tt.Refs) != tt.Samples {
				t.Fatalf("seq %d refs=%d samples=%d", st.Seq, len(tt.Refs), tt.Samples)
			}
			for _, r := range tt.Refs {
				if r.SHA256 == "" || r.Offset <= 0 || r.Size == 0 {
					t.Fatalf("bad sample ref %+v", r)
				}
			}
		}
	}
	if !foundRefs {
		t.Fatal("no per-sample provenance refs recorded")
	}
}

func TestIndexParamsMismatch(t *testing.T) {
	dir := e2eFixture(t)
	out := filepath.Join(dir, "out")
	idx := filepath.Join(dir, "idx")
	in := filepath.Join(dir, "clean")
	if _, err := Run(Config{Input: in, Output: out, IndexPath: idx, Chunk: 16}); err != nil {
		t.Fatal(err)
	}
	// same input but different chunk size must be refused
	if _, err := Run(Config{Input: in, Output: out, IndexPath: idx, Chunk: 32}); err == nil {
		t.Fatal("expected params mismatch error")
	}
}

func TestCorruptIndexTailRun(t *testing.T) {
	dir := e2eFixture(t)
	out := filepath.Join(dir, "out")
	idx := filepath.Join(dir, "idx")
	in := filepath.Join(dir, "clean")
	cfg := Config{Input: in, Output: out, IndexPath: idx, Chunk: 16, StopAfter: 2}
	if _, err := Run(cfg); !errors.Is(err, ErrInterrupted) {
		t.Fatalf("expected interrupt, got %v", err)
	}
	// tear the index tail as if the writer crashed mid-append
	f, _ := os.OpenFile(idx, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString("garbage-tail")
	f.Close()
	rep, err := Run(Config{Input: in, Output: out, IndexPath: idx, Chunk: 16})
	if err != nil {
		t.Fatal(err)
	}
	if !rep.CorruptIndexTail {
		t.Fatal("corrupt tail not reported")
	}
	if _, err := Verify(VerifyConfig{Input: in, Output: out, IndexPath: idx}); err != nil {
		t.Fatal(err)
	}
}
