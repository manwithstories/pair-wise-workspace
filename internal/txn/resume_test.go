package txn

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestResumeMatchesCleanRun(t *testing.T) {
	dir, r, plan := prepare(t)
	defer r.Close()
	work := filepath.Join(dir, "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
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
