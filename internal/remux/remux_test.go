package remux_test

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"

	"mediarun/internal/ingest"
	"mediarun/internal/mapping"
	"mediarun/internal/remux"
	"mediarun/internal/timebase"
)

func planFor(t *testing.T) *mapping.Plan {
	t.Helper()
	tracks := []ingest.TrackDesc{
		{ID: "video/0", Kind: ingest.KindVideo, Codec: "avc1", Timescale: 30000, Width: 320, Height: 180, SampleCount: 3},
		{ID: "audio/0", Kind: ingest.KindAudio, Codec: "mp4a", Timescale: 48000, Channels: 2, SampleCount: 2},
	}
	cfg := mapping.Config{Rules: []mapping.Rule{
		{Target: mapping.RoleVideo, Source: "video/0"},
		{Target: mapping.RoleAudio, Source: "audio/0"},
	}}
	plan, err := mapping.Build(cfg, tracks)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	return plan
}

func sampleEvent(track string, idx int64, payload byte, size int) *ingest.SampleEvent {
	buf := make([]byte, size)
	for i := range buf {
		buf[i] = payload ^ byte(i)
	}
	return &ingest.SampleEvent{
		TrackID: track, Index: idx, DataOffset: int64(idx * 100), Size: int64(size),
		DTS: idx, PTS: idx, Duration: 1, Timebase: timebase.NewTimebase(30000),
		Payload: bytes.NewReader(buf),
	}
}

func writeBundle(t *testing.T, dest string) string {
	t.Helper()
	w, err := remux.NewWriter(dest, planFor(t))
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	events := []*ingest.SampleEvent{
		sampleEvent("video/0", 0, 0x10, 64),
		sampleEvent("video/0", 1, 0x20, 64),
		sampleEvent("audio/0", 0, 0x30, 32),
		sampleEvent("video/0", 2, 0x40, 64),
		sampleEvent("audio/0", 1, 0x50, 32),
	}
	for _, ev := range events {
		if err := w.WriteSample(ev); err != nil {
			w.Abort()
			t.Fatalf("WriteSample: %v", err)
		}
	}
	hash, err := w.Commit()
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if w.Samples() != 5 {
		t.Fatalf("samples = %d, want 5", w.Samples())
	}
	return hash
}

func TestRemuxRoundTrip(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "out.mrmx")
	writeBundle(t, dest)
	r, err := remux.OpenReader(dest)
	if err != nil {
		t.Fatalf("OpenReader: %v", err)
	}
	defer r.Close()
	if len(r.TrackDescs()) != 2 || r.Header.Version != 1 {
		t.Fatalf("header = %+v", r.Header)
	}
	count := 0
	var firstPayload []byte
	for {
		fr, err := r.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		payload, err := io.ReadAll(fr.Payload)
		if err != nil {
			t.Fatalf("payload: %v", err)
		}
		if int64(len(payload)) != fr.Size {
			t.Fatalf("payload len %d != frame size %d", len(payload), fr.Size)
		}
		if count == 0 {
			firstPayload = payload
			if fr.TrackID != "video/0" || fr.Index != 0 {
				t.Fatalf("first frame = %+v", fr)
			}
		}
		count++
	}
	if count != 5 {
		t.Fatalf("frames = %d, want 5", count)
	}
	if err := r.VerifyEnding(); err != nil {
		t.Fatalf("VerifyEnding: %v", err)
	}
	if r.FrameCount() != 5 {
		t.Fatalf("FrameCount = %d, want 5", r.FrameCount())
	}
	if firstPayload[0] != 0x10 {
		t.Fatalf("payload bytes were not copied losslessly: %x", firstPayload[:4])
	}
}

func TestDeterministicOutputHash(t *testing.T) {
	dir := t.TempDir()
	h1 := writeBundle(t, filepath.Join(dir, "a.mrmx"))
	h2 := writeBundle(t, filepath.Join(dir, "b.mrmx"))
	if h1 == "" || h1 != h2 {
		t.Fatalf("hashes must be identical: %q vs %q", h1, h2)
	}
	b1, _ := os.ReadFile(filepath.Join(dir, "a.mrmx"))
	b2, _ := os.ReadFile(filepath.Join(dir, "b.mrmx"))
	if !bytes.Equal(b1, b2) {
		t.Fatal("byte-identical inputs must produce byte-identical bundles")
	}
}

func TestAtomicCommitLeavesNoTempFiles(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "out.mrmx")
	writeBundle(t, dest)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) == "" || bytes.HasPrefix([]byte(e.Name()), []byte(".")) {
			t.Fatalf("staging temp file left behind: %s", e.Name())
		}
	}
	if _, err := os.Stat(dest); err != nil {
		t.Fatalf("committed destination missing: %v", err)
	}
}

func TestAbortDoesNotCreateDestination(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "never.mrmx")
	w, err := remux.NewWriter(dest, planFor(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := w.WriteSample(sampleEvent("video/0", 0, 1, 16)); err != nil {
		t.Fatal(err)
	}
	w.Abort()
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatalf("destination must not exist after abort, stat err=%v", err)
	}
}

func TestRejectTruncatedBundle(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "out.mrmx")
	writeBundle(t, dest)
	raw, _ := os.ReadFile(dest)
	cut := filepath.Join(dir, "cut.mrmx")
	if err := os.WriteFile(cut, raw[:len(raw)/2], 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := remux.OpenReader(cut)
	if err != nil {
		return // header-level rejection is acceptable
	}
	defer r.Close()
	for {
		fr, err := r.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return // payload-level truncation also acceptable
		}
		if _, err := io.ReadAll(fr.Payload); err != nil {
			return
		}
	}
	// Clean EOF before the promised frame count must be flagged as truncation.
	if err := r.VerifyEnding(); err == nil {
		t.Fatal("expected VerifyEnding to detect the missing frames")
	}
}
