package txn

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"mediamerge/internal/journal"
	"mediamerge/internal/model"
)

func TestResumeMatchesCleanRun(t *testing.T) {
	dir, r, plan := prepare(t)
	defer r.Close()
	work := filepath.Join(dir, "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := copyInput(work, r.Path); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(context.Background(), work, r, plan, Options{Queue: 1, InterruptAfter: 2, CrashBytes: 3}); !errors.Is(err, ErrInterrupted) {
		t.Fatalf("interrupt: %v", err)
	}
	start := time.Now()
	res, err := Resume(context.Background(), work, r, plan, 1)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if elapsed > time.Second {
		t.Fatalf("resume took %v", elapsed)
	}
	if res.RecoveredStage != 2 {
		t.Fatalf("recovered stage=%d", res.RecoveredStage)
	}
	if res.RollbackBytes <= 0 {
		t.Fatalf("expected rollback of injected bytes, got %d", res.RollbackBytes)
	}
	if !res.SampleMatch {
		t.Fatal("resumed output sample mismatch")
	}
	clean := filepath.Join(dir, "clean")
	os.MkdirAll(clean, 0o755)
	want, err := Run(context.Background(), clean, r, plan, Options{Queue: 4})
	if err != nil {
		t.Fatalf("clean: %v", err)
	}
	if res.OutputSHA != want.OutputSHA {
		t.Fatalf("resume sha %s != clean %s", res.OutputSHA, want.OutputSHA)
	}
	// Second resume is a no-op and keeps the same digest.
	res2, err := Resume(context.Background(), work, r, plan, 1)
	if err != nil {
		t.Fatalf("second resume: %v", err)
	}
	if res2.OutputSHA != res.OutputSHA || res2.RollbackBytes != 0 || !res2.AlreadyFinal {
		t.Fatalf("second resume changed output: %+v", res2)
	}
}

func TestRecoverRejectsTamperedOutput(t *testing.T) {
	dir, r, plan := prepare(t)
	defer r.Close()
	work := filepath.Join(dir, "work")
	os.MkdirAll(work, 0o755)
	if err := copyInput(work, r.Path); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(context.Background(), work, r, plan, Options{Queue: 2, InterruptAfter: 1, CrashBytes: 0}); !errors.Is(err, ErrInterrupted) {
		t.Fatalf("interrupt: %v", err)
	}
	out, _ := Paths(work)
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	data[40] ^= 0xff // corrupt a committed payload byte
	if err := os.WriteFile(out, data, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = Recover(work, r.InputSHA, r.Size, plan)
	if err == nil {
		t.Fatal("expected digest mismatch")
	}
}

// TestJournalOutputMismatchIsRejected covers the fourth fault class: the
// journal itself disagrees with the bytes on disk. Recovery must refuse to
// trust the commit rather than resuming over a corrupt prefix.
func TestJournalOutputMismatchIsRejected(t *testing.T) {
	dir, r, plan := prepare(t)
	defer r.Close()
	work := filepath.Join(dir, "work")
	os.MkdirAll(work, 0o755)
	if err := copyInput(work, r.Path); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(context.Background(), work, r, plan, Options{Queue: 2, InterruptAfter: 2, CrashBytes: -1}); !errors.Is(err, ErrInterrupted) {
		t.Fatalf("interrupt: %v", err)
	}
	// Rewrite the last complete commit record with a bogus digest.
	_, jpath := Paths(work)
	data, err := os.ReadFile(jpath)
	if err != nil {
		t.Fatal(err)
	}
	recs, err := journal.Load(jpath)
	if err != nil {
		t.Fatal(err)
	}
	last := recs[len(recs)-1]
	line := fmt.Sprintf(`{"stage":%d,"kind":"%s","input_sha256":"%s","input_size":%d,"output_len":%d,"prefix_sha256":"%s"}`,
		last.Stage, journal.KindCommit, last.InputSHA256, last.InputSize, last.OutputLen,
		"0000000000000000000000000000000000000000000000000000000000000000")
	_ = data
	if err := os.WriteFile(jpath, []byte(line+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Recover(work, r.InputSHA, r.Size, plan); err == nil {
		t.Fatal("recovery accepted a journal/output mismatch")
	}
}

// TestTruncatedOutputRejectedInRecovery covers the tail-truncation fault as it
// crosses the transaction boundary: after a complete run, losing output bytes
// must be caught by the journal length/digest check, not silently accepted.
func TestTruncatedOutputRejectedInRecovery(t *testing.T) {
	dir, r, plan := prepare(t)
	defer r.Close()
	if err := copyInput(dir, r.Path); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(context.Background(), dir, r, plan, Options{Queue: 2, InterruptAfter: -1}); err != nil {
		t.Fatal(err)
	}
	out, _ := Paths(dir)
	info, err := os.Stat(out)
	if err != nil {
		t.Fatal(err)
	}
	cut := info.Size() - 4096
	if err := os.Truncate(out, cut); err != nil {
		t.Fatal(err)
	}
	_, err = Recover(dir, r.InputSHA, r.Size, plan)
	if err == nil {
		t.Fatal("recovery accepted a truncated output")
	}
	pe, ok := err.(*model.Error)
	if !ok || pe.Code != model.CodeIntegrity {
		t.Fatalf("err=%v", err)
	}
	if pe.Offset != cut {
		t.Fatalf("offset=%d want %d", pe.Offset, cut)
	}
}

// TestRollbackToEarlierStage exercises per-stage rollback: after a full run we
// rewind to stage 1's boundary, resume, and still get the identical output.
func TestRollbackToEarlierStage(t *testing.T) {
	dir, r, plan := prepare(t)
	defer r.Close()
	if err := copyInput(dir, r.Path); err != nil {
		t.Fatal(err)
	}
	full, err := Run(context.Background(), dir, r, plan, Options{Queue: 2, InterruptAfter: -1})
	if err != nil {
		t.Fatal(err)
	}
	recs, err := journal.Load(filepath.Join(dir, "stages.log"))
	if err != nil {
		t.Fatal(err)
	}
	var stage1 int64 = -1
	for _, rec := range recs {
		if rec.Kind == journal.KindCommit && rec.Stage == 1 {
			stage1 = rec.OutputLen
		}
	}
	if stage1 < 0 {
		t.Fatal("no stage 1 commit")
	}
	// Simulate a crash that leaves junk after stage 1, then recover.
	out, _ := Paths(dir)
	f, err := os.OpenFile(out, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(make([]byte, 1000)); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if err := os.Truncate(out, stage1+1000); err != nil {
		t.Fatal(err)
	}
	// Rewrite the journal to keep only records up to stage 1.
	var keep []journal.Record
	for _, rec := range recs {
		if rec.Stage <= 1 {
			keep = append(keep, rec)
		}
	}
	if _, err := journal.Rewrite(filepath.Join(dir, "stages.log"), keep, nil); err != nil {
		t.Fatal(err)
	}
	res, err := Resume(context.Background(), dir, r, plan, 1)
	if err != nil {
		t.Fatalf("resume after rewind: %v", err)
	}
	if res.RecoveredStage != 1 || res.RollbackBytes != 1000 {
		t.Fatalf("res=%+v", res)
	}
	if res.OutputSHA != full.OutputSHA {
		t.Fatalf("rewound output %s != full %s", res.OutputSHA, full.OutputSHA)
	}
}
