package remux

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"mediacheck/internal/clock"
	"mediacheck/internal/ingest"
	"mediacheck/internal/model"
)

func buildFixture(t *testing.T, frames int) *model.Fixture {
	t.Helper()
	cfg := ingest.DefaultConfig(17)
	cfg.VideoFrames = frames
	g, err := ingest.Generate(cfg)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	return g.Fixture()
}

func writeAll(t *testing.T, dest string, fx *model.Fixture) string {
	t.Helper()
	w, err := Open(dest, fx.Streams)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer w.Abort()
	for _, p := range fx.Packets {
		if err := w.Write(context.Background(), p); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	sum, err := w.Commit()
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	return sum
}

func TestCommitProducesReadableMatroska(t *testing.T) {
	if testing.Short() {
		t.Skip("requires FFmpeg muxing")
	}
	dir := t.TempDir()
	dest := filepath.Join(dir, "out.mkv")
	fx := buildFixture(t, 50)
	sum := writeAll(t, dest, fx)

	data, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	// Matroska files start with the EBML magic.
	if len(data) < 4 || data[0] != 0x1A || data[1] != 0x45 || data[2] != 0xDF || data[3] != 0xA3 {
		t.Fatalf("output does not start with EBML magic: %x", data[:4])
	}
	want := sha256.Sum256(data)
	if got := hex.EncodeToString(want[:]); got != sum {
		t.Fatalf("returned hash %s != file hash %s", sum, got)
	}
	// No temporary files may survive a successful commit.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "out.mkv" {
			t.Fatalf("stray file %q left behind", e.Name())
		}
	}
}

func TestRebuildIsByteIdentical(t *testing.T) {
	if testing.Short() {
		t.Skip("requires FFmpeg muxing")
	}
	dir := t.TempDir()
	fx := buildFixture(t, 40)
	sumA := writeAll(t, filepath.Join(dir, "a.mkv"), fx)
	sumB := writeAll(t, filepath.Join(dir, "b.mkv"), fx)
	if sumA != sumB {
		t.Fatalf("rebuild hashes differ: %s vs %s", sumA, sumB)
	}
}

func TestAbortLeavesDestinationUntouchedAndNoTempFile(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "out.mkv")
	if err := os.WriteFile(dest, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	fx := buildFixture(t, 10)
	w, err := Open(dest, fx.Streams)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Write(context.Background(), fx.Packets[0]); err != nil {
		t.Fatal(err)
	}
	w.Abort()
	w.Abort() // idempotent

	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "original" {
		t.Fatalf("destination was modified by an aborted run: %q", got)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "out.mkv" {
			t.Fatalf("temporary file %q survived Abort", e.Name())
		}
	}
}

func TestWriteHonoursCancellation(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "out.mkv")
	fx := buildFixture(t, 10)
	w, err := Open(dest, fx.Streams)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Abort()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err = w.Write(ctx, fx.Packets[0])
	if !errors.Is(err, ErrAborted) {
		t.Fatalf("Write after cancel = %v, want ErrAborted", err)
	}
	if _, err := w.Commit(); err == nil {
		t.Fatal("Commit after a cancelled write should not succeed silently")
	}
	if _, statErr := os.Stat(dest); !os.IsNotExist(statErr) {
		t.Fatal("cancelled run left a destination file")
	}
}

func TestCommitTwiceFails(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "out.mkv")
	fx := buildFixture(t, 5)
	w, err := Open(dest, fx.Streams)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Abort()
	for _, p := range fx.Packets {
		if err := w.Write(context.Background(), p); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := w.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Commit(); err == nil {
		t.Fatal("second Commit should fail")
	}
}

func TestOpenRejectsMissingStreams(t *testing.T) {
	dir := t.TempDir()
	_, err := Open(filepath.Join(dir, "x.mkv"), nil)
	if err == nil {
		t.Fatal("expected error for fixture without streams")
	}
}

func TestOpenRejectsInvalidStreamInfo(t *testing.T) {
	dir := t.TempDir()
	bad := []model.StreamInfo{{
		Track: model.TrackAudio, Codec: model.CodecOpus,
		Timebase: clock.Timebase{}, SampleRate: 48000, Channels: 2,
	}}
	if _, err := Open(filepath.Join(dir, "x.mkv"), bad); err == nil {
		t.Fatal("expected error for invalid stream timebase")
	}
}

func TestAbortDuringWritesReleasesEverything(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "out.mkv")
	fx := buildFixture(t, 200)
	w, err := Open(dest, fx.Streams)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 40; i++ {
		if err := w.Write(context.Background(), fx.Packets[i]); err != nil {
			t.Fatal(err)
		}
	}
	w.Abort()
	// Writing after Abort must fail cleanly, not touch freed FFmpeg memory.
	if err := w.Write(context.Background(), fx.Packets[41]); err == nil {
		t.Fatal("Write after Abort succeeded")
	}
	if _, err := w.Commit(); err == nil {
		t.Fatal("Commit after Abort succeeded")
	}
}
