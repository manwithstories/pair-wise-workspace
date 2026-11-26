package store

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"pairflow/internal/model"
)

func TestCASRevalidatesProvenanceAndDetectsCorruptBlob(t *testing.T) {
	cas, err := NewCAS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	keyInputs := CacheKeyInputs{SourceFingerprint: "src", InputDigest: "in", NormalizedMeta: "meta", Stage: "result"}
	key := CacheKey(keyInputs)
	digest, err := cas.PutBlob([]byte("output"))
	if err != nil {
		t.Fatal(err)
	}
	if err := cas.Put(Entry{Key: key, Inputs: keyInputs, SourceFingerprint: "src", InputDigest: "in", OutputDigest: digest}); err != nil {
		t.Fatal(err)
	}
	if _, blob, merr := cas.GetRevalidated(key, "src", "in"); merr != nil || string(blob) != "output" {
		t.Fatalf("hit blob=%q merr=%v", blob, merr)
	}
	if _, _, merr := cas.GetRevalidated(key, "unknown", "in"); merr == nil || merr.Code != model.CodeCacheMismatch {
		t.Fatalf("source mismatch merr=%v", merr)
	}

	blobPath := cas.blobPath(digest)
	if err := os.WriteFile(blobPath, []byte("tampered"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, merr := cas.GetRevalidated(key, "src", "in")
	if merr == nil || merr.Code != model.CodeCacheMismatch {
		t.Fatalf("corrupt blob merr=%v", merr)
	}
}

func TestRecoverCheckpointSkipsCorruptMain(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "output.jsonl")
	if err := os.WriteFile(out, []byte("abc"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := SaveCheckpoint(dir, Checkpoint{Stage: "records", CompletedRecords: 3, OutputPath: out, OutputBytes: 3, OutputSHA256: "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "checkpoint.json"), []byte("{bad"), 0o644); err != nil {
		t.Fatal(err)
	}
	cp, merr := RecoverCheckpoint(dir, []string{"records"})
	if merr != nil {
		t.Fatal(merr)
	}
	if cp == nil || cp.CompletedRecords != 3 {
		t.Fatalf("checkpoint=%+v", cp)
	}
}

func TestExitCodeClassification(t *testing.T) {
	if got := model.ExitCode(&model.MediaError{Code: model.CodeInvalidArgument}); got != model.ExitUsage {
		t.Fatalf("usage=%d", got)
	}
	if got := model.ExitCode(&model.MediaError{Code: model.CodeDigestMismatch}); got != model.ExitMediaOrSource {
		t.Fatalf("media=%d", got)
	}
	if got := model.ExitCode(errors.New("boom")); got != model.ExitInternal {
		t.Fatalf("internal=%d", got)
	}
}
