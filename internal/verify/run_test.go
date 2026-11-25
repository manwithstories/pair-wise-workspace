package verify

import (
	"context"
	"errors"
	"testing"
)

func TestRunCrashResumeVerification(t *testing.T) {
	res, err := Run(context.Background(), Config{
		Seed:          73,
		Faults:        []string{"pcr-wrap", "cc-gap", "psi-update", "bad-crc", "truncated"},
		BufferPackets: 16,
		ConsumerDelay: 0,
		CrashAfter:    2,
		Resume:        true,
		WorkDir:       t.TempDir(),
		SilenceMS:     80,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Roundtrip != 100 || res.KeyframeHit != 100 || res.HashRepeat != 100 {
		t.Fatalf("rates roundtrip=%v keyframe=%v repeat=%v", res.Roundtrip, res.KeyframeHit, res.HashRepeat)
	}
	if res.BadCRC != 1 || res.TruncatedCommitted != 0 || res.WALReplayed != 1 {
		t.Fatalf("counts badCRC=%d truncated=%d wal=%d", res.BadCRC, res.TruncatedCommitted, res.WALReplayed)
	}
	if res.DuplicateSources != 1 {
		t.Fatalf("duplicate sources=%d, want 1", res.DuplicateSources)
	}
	if len(res.Clips) != 4 || len(res.NormalHashes) != 4 || len(res.ResumeHashes) != 4 {
		t.Fatalf("clips=%d normal=%d resume=%d", len(res.Clips), len(res.NormalHashes), len(res.ResumeHashes))
	}
	if res.NormalHashes[0] != res.ResumeHashes[0] {
		t.Fatal("normal and resume hashes differ")
	}
	if res.NormalHashes[0] != res.NormalHashes[3] {
		t.Fatal("fixture duplicate clips should have identical canonical hashes")
	}
	if res.PeakHeapMiB <= 0 || res.PeakHeapMiB > 32 {
		t.Fatalf("peak heap %.2f MiB outside bound", res.PeakHeapMiB)
	}
}

func TestValidateMapsBadArguments(t *testing.T) {
	err := Validate(Config{BufferPackets: 4096, CrashAfter: 2, SilenceMS: 80, Faults: []string{"nope"}})
	var exitErr *ExitError
	if !errors.As(err, &exitErr) || exitErr.Code != ExitBadArgs {
		t.Fatalf("err=%v, want exit code 2", err)
	}
}

func TestResumeRequiredAfterCrash(t *testing.T) {
	res, err := Run(context.Background(), Config{
		Seed: 73, Faults: []string{"bad-crc"}, BufferPackets: 8,
		CrashAfter: 2, Resume: false, WorkDir: t.TempDir(), SilenceMS: 80,
	})
	var exitErr *ExitError
	if !errors.As(err, &exitErr) || exitErr.Code != ExitInvariant {
		t.Fatalf("err=%v, want exit code 4", err)
	}
	if res == nil || res.WALReplayed != 0 {
		t.Fatalf("res=%+v", res)
	}
}
