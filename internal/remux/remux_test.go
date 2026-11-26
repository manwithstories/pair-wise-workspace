package remux

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mediaflow/internal/clock"
	"mediaflow/internal/quality"
	"mediaflow/internal/timeline"
)

func TestAtomicWriteAndChecksum(t *testing.T) {
	dir := t.TempDir()
	w, err := NewWriter(dir)
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("deterministic remux payload")
	res, err := w.WriteAtomically("out/stream.bin", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(res.Path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatal("written payload mismatch")
	}
	// Checksum must match an independent hash.
	if res.SHA256 != shaHex(payload) {
		t.Fatal("checksum mismatch")
	}
	// No temp files left behind.
	entries, _ := os.ReadDir(filepath.Join(dir, "out"))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".mediaflow-") {
			t.Fatal("temp file not cleaned up")
		}
	}
}

func TestPathEscapeRejected(t *testing.T) {
	w, _ := NewWriter(t.TempDir())
	for _, name := range []string{"../escape.bin", "a/../../b.bin", "/etc/passwd"} {
		if _, err := w.WriteAtomically(name, strings.NewReader("x")); !errors.Is(err, timeline.ErrPathEscape) {
			t.Fatalf("name %q not rejected: %v", name, err)
		}
	}
}

func TestSizeLimitRejected(t *testing.T) {
	w, _ := NewWriter(t.TempDir())
	big := bytes.NewReader(make([]byte, MaxOutputBytes+1))
	if _, err := w.WriteAtomically("big.bin", big); !errors.Is(err, timeline.ErrLimit) {
		t.Fatalf("oversized output not rejected: %v", err)
	}
	// Destination must not exist after a rejected write.
	if _, err := os.Stat(filepath.Join(w.Root(), "big.bin")); !os.IsNotExist(err) {
		t.Fatal("partial output left behind after limit rejection")
	}
}

func TestEncodeFramesDeterministic(t *testing.T) {
	frames := []clock.Normalized{
		{Track: 1, Seq: 0, StartUS: 0, Duration: 40_000, Payload: []byte{1, 2}},
		{Track: 1, Seq: 1, StartUS: 40_000, Duration: 36_000, Recovered: true, Payload: []byte{3}},
		{Track: 2, Seq: 0, StartUS: 0, Duration: 21_333, Payload: []byte{9, 8, 7}},
	}
	a, sumA := EncodeFrames(frames)
	b, sumB := EncodeFrames(frames)
	if !bytes.Equal(a, b) || sumA != sumB {
		t.Fatal("frame encoding not deterministic")
	}
	if len(a) == 0 {
		t.Fatal("empty encoding")
	}
}

func TestEncodeSegmentsDeterministic(t *testing.T) {
	segs := []quality.Segment{
		{Index: 0, StartUS: 0, EndUS: 1000, StartFrame: 0, EndFrame: 2},
		{Index: 1, StartUS: 1000, EndUS: 2000, StartFrame: 2, EndFrame: 4},
	}
	a := EncodeSegments(segs)
	b := EncodeSegments(segs)
	if !bytes.Equal(a, b) {
		t.Fatal("segment manifest not deterministic")
	}
	if len(a) != len(segs)*5*8 {
		t.Fatalf("manifest size %d unexpected", len(a))
	}
}

func shaHex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
