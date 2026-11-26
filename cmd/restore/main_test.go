package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/archive/tsrestore/internal/rtp"
	"github.com/archive/tsrestore/internal/sample"
	"github.com/archive/tsrestore/internal/timeline"
)

func TestClassifyExitCodes(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"integrity", timeline.IntegrityError("cache.get", "mismatch", nil), exitIntegrity},
		{"checkpoint", timeline.NewError(timeline.ClassFatal, "x", "y", timeline.ErrCheckpoint), exitIntegrity},
		{"fatal", timeline.NewError(timeline.ClassFatal, "rtp", "unrecoverable", nil), exitFatal},
		{"corrupt", timeline.NewError(timeline.ClassCorrupt, "rtp", "bad", nil), exitFatal},
	}
	for _, tc := range cases {
		if got := classify(tc.err); got != tc.want {
			t.Fatalf("%s: classify=%d want %d", tc.name, got, tc.want)
		}
	}
}

// A truncated stream yields a Corrupt error from the ordered reader, which is
// the class of unrecoverable input mapped to exit code 2.
func TestTruncatedInputIsFatalClass(t *testing.T) {
	stream, err := sample.BuildEdgeFixture(sample.EdgeTruncated, 41, 12)
	if err != nil {
		t.Fatal(err)
	}
	reader := rtp.NewOrderedReader(bytes.NewReader(stream))
	var corrupt bool
	for {
		_, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			var te *timeline.Error
			if errors.As(err, &te) && te.Class == timeline.ClassCorrupt {
				corrupt = true
			}
			break
		}
	}
	if !corrupt {
		t.Fatal("truncated input must produce a classified Corrupt error (exit 2 path)")
	}
}

// A checkpoint written under one secret must be rejected under another with an
// integrity error, exercising the exit code 3 path.
func TestCheckpointTamperIsIntegrityClass(t *testing.T) {
	dir := t.TempDir()
	if _, _, err := openStoresWithSecret(dir, []byte("right-secret")); err != nil {
		t.Fatal(err)
	}
	// Persist at least one committed segment so a document exists.
	_, ckpt, err := openStoresWithSecret(dir, []byte("right-secret"))
	if err != nil {
		t.Fatal(err)
	}
	if err := ckpt.CommitSegment("v1", 0, "h0", 0, 1); err != nil {
		t.Fatal(err)
	}
	if _, _, err := openStoresWithSecret(dir, []byte("wrong-secret")); !errors.Is(err, timeline.ErrIntegrity) {
		t.Fatalf("tampered checkpoint must map to integrity (exit 3), got %v", err)
	}
}

func TestFixtureSubcommandWritesFile(t *testing.T) {
	dir := t.TempDir()
	code := runFixture([]string{"--out", dir, "--seed", "41", "truncated"})
	if code != exitOK {
		t.Fatalf("fixture exit = %d, want 0", code)
	}
	matches, err := filepath.Glob(filepath.Join(dir, "fixture-*.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 {
		t.Fatalf("expected one fixture file, got %v", matches)
	}
	if info, err := os.Stat(matches[0]); err != nil || info.Size() == 0 {
		t.Fatal("fixture file missing or empty")
	}
}

func TestParseVerifyRequiresOut(t *testing.T) {
	if _, err := parseVerify([]string{"--seed", "1"}); err == nil {
		t.Fatal("missing --out must be rejected")
	}
}
