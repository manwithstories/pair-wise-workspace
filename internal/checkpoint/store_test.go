package checkpoint

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cp.json")
	in := DigestBytes([]byte("input-bytes"))
	params := DigestParams([]byte(`{"seed":42,"frames":1200}`))
	s := New(in, params)
	if err := s.Append(Segment{ID: 0, Start: 0, End: 100, PartialDigest: "a"}, path); err != nil {
		t.Fatal(err)
	}
	if err := s.Append(Segment{ID: 1, Start: 100, End: 200, PartialDigest: "b"}, path); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path, in, params)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(got.Segments) != 2 || got.CompletedUpTo() != 200 {
		t.Fatalf("segments wrong: %+v", got)
	}
	if got.FrameCount != 200 {
		t.Fatalf("frame count=%d want 200", got.FrameCount)
	}
	if !got.IsSegmentDone(1) || got.IsSegmentDone(9) {
		t.Fatal("IsSegmentDone wrong")
	}
	// No temp file should be left behind after atomic save.
	if _, err := os.Stat(path + ".tmp"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temp file leaked: %v", err)
	}
}

func TestResumeRejectsMismatch(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cp.json")
	in := DigestBytes([]byte("input"))
	params := DigestParams([]byte("params"))
	s := New(in, params)
	if err := s.Append(Segment{ID: 0, Start: 0, End: 50, PartialDigest: "x"}, path); err != nil {
		t.Fatal(err)
	}
	// Different input digest must be refused.
	if _, err := Load(path, DigestBytes([]byte("tampered")), params); !errors.Is(err, ErrMismatch) {
		t.Fatalf("input mismatch err=%v", err)
	}
	// Different parameter digest must be refused.
	if _, err := Load(path, in, DigestParams([]byte("other-params"))); !errors.Is(err, ErrMismatch) {
		t.Fatalf("params mismatch err=%v", err)
	}
	// Matching digests resume successfully and skip the finished segment.
	got, err := Load(path, in, params)
	if err != nil {
		t.Fatalf("matching load: %v", err)
	}
	if got.CompletedUpTo() != 50 {
		t.Fatalf("resume position wrong: %d", got.CompletedUpTo())
	}
}

func TestMalformedAndAtomicity(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cp.json")
	if err := os.WriteFile(path, []byte("{not-json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path, "a", "b"); !errors.Is(err, ErrMalformed) {
		t.Fatalf("malformed err=%v", err)
	}
	// Out-of-order append is rejected.
	s := New("a", "b")
	if err := s.Append(Segment{ID: 2, Start: 0, End: 10, PartialDigest: "x"}, path); err != nil {
		t.Fatal(err)
	}
	if err := s.Append(Segment{ID: 1, Start: 10, End: 20, PartialDigest: "y"}, path); err == nil {
		t.Fatal("out-of-order segment must be rejected")
	}
}

func TestDigestStability(t *testing.T) {
	// Same bytes always produce the same 64-hex-char SHA-256.
	d1 := DigestBytes([]byte("deterministic"))
	d2 := DigestBytes([]byte("deterministic"))
	if d1 != d2 || len(d1) != 64 {
		t.Fatalf("digest unstable or wrong length: %q", d1)
	}
	if DigestBytes([]byte("other")) == d1 {
		t.Fatal("distinct inputs must differ")
	}
}

func TestCompletedUpToHandlesGaps(t *testing.T) {
	s := New("in", "p")
	s.Segments = []Segment{
		{ID: 0, Start: 0, End: 100},
		{ID: 2, Start: 200, End: 300}, // gap: 100..200 missing
	}
	if got := s.CompletedUpTo(); got != 100 {
		t.Fatalf("contiguous upTo=%d want 100 (gap must stop it)", got)
	}
}
