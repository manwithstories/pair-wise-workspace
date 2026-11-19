package index

import (
	"crypto/sha256"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"mediq/internal/fixture"
)

// readValidFrames parses a fixture sequence and returns only the frames whose
// payload still matches the recorded SHA-256, which is exactly the projection
// `process` indexes.
func readValidFrames(t *testing.T, path string) []struct {
	seq     uint64
	payload []byte
} {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []struct {
		seq     uint64
		payload []byte
	}
	for off := 0; off+72 <= len(data); {
		hdr := data[off : off+72]
		if string(hdr[0:4]) != "MFRM" {
			break
		}
		metaLen := binary.BigEndian.Uint32(hdr[32:36])
		frameLen := binary.BigEndian.Uint32(hdr[36:40])
		body := off + 72
		end := body + int(metaLen) + int(frameLen)
		if end > len(data) {
			break
		}
		payload := data[body+int(metaLen) : end]
		want := sha256.Sum256(payload)
		if string(want[:]) == string(hdr[40:72]) {
			out = append(out, struct {
				seq     uint64
				payload []byte
			}{binary.BigEndian.Uint64(hdr[8:16]), payload})
		}
		off = end
	}
	return out
}

// buildProjection writes an index containing exactly the valid frames of a
// fixture, in source order, the way `process` would.
func buildProjection(t *testing.T, input, out string) {
	t.Helper()
	w, err := Create(out, filepath.Join(filepath.Dir(out), "tbl"), 8)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Abort()
	var media int64
	for _, fr := range readValidFrames(t, input) {
		media += 3000
		if err := w.Append(fr.seq, media, fr.payload); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := w.CloseChunk(); err != nil {
		t.Fatal(err)
	}
	if err := w.Finish(); err != nil {
		t.Fatal(err)
	}
	if err := w.Publish(); err != nil {
		t.Fatal(err)
	}
}

func TestVerifySourceAcceptsExactProjection(t *testing.T) {
	dir := t.TempDir()
	res, err := fixture.Generate(fixture.Config{Dir: dir, Frames: 40, FrameBytes: 64, Corrupt: 11, Seed: 2})
	if err != nil {
		t.Fatal(err)
	}
	input := res.Files["damaged"]
	out := filepath.Join(dir, "out.mp4")
	buildProjection(t, input, out)

	rep, err := VerifyFile(out, VerifyOptions{SourcePath: input})
	if err != nil {
		t.Fatal(err)
	}
	if !rep.IndexValid {
		t.Fatalf("valid projection rejected: %v", rep.Problems)
	}
	if rep.Samples != 39 {
		t.Fatalf("samples %d", rep.Samples)
	}
	if len(rep.SourceFaults) != 1 || rep.SourceFaults[0].Code != "hash_mismatch" {
		t.Fatalf("source faults %+v", rep.SourceFaults)
	}
	if rep.UnindexedSource != 0 {
		t.Fatalf("unindexed source frames %d", rep.UnindexedSource)
	}
}

func TestVerifySourceDetectsSubstitutedSample(t *testing.T) {
	dir := t.TempDir()
	res, err := fixture.Generate(fixture.Config{Dir: dir, Frames: 40, FrameBytes: 64, Seed: 2})
	if err != nil {
		t.Fatal(err)
	}
	input := res.Files["normal"]
	out := filepath.Join(dir, "out.mp4")
	buildProjection(t, input, out)

	tbl, err := ReadTable(out)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(out, os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	bad := make([]byte, tbl.SampleSize[5])
	for i := range bad {
		bad[i] = 0xEE
	}
	if _, err := f.WriteAt(bad, int64(tbl.Offsets[5])); err != nil {
		t.Fatal(err)
	}
	f.Close()

	rep, err := VerifyFile(out, VerifyOptions{SourcePath: input})
	if err != nil {
		t.Fatal(err)
	}
	if rep.IndexValid {
		t.Fatal("substituted sample passed verification")
	}
	if len(rep.Problems) == 0 {
		t.Fatal("no problem reported")
	}
}

// TestVerifySourceDetectsDroppedFrame proves a valid source frame missing from
// the index is reported rather than ignored.
func TestVerifySourceDetectsDroppedFrame(t *testing.T) {
	dir := t.TempDir()
	res, err := fixture.Generate(fixture.Config{Dir: dir, Frames: 20, FrameBytes: 32, Seed: 4})
	if err != nil {
		t.Fatal(err)
	}
	input := res.Files["normal"]

	// Build a truncated projection: only every other valid frame.
	out := filepath.Join(dir, "gap.mp4")
	w, err := Create(out, filepath.Join(dir, "tbl2"), 4)
	if err != nil {
		t.Fatal(err)
	}
	var media int64
	for i, fr := range readValidFrames(t, input) {
		if i%2 == 1 {
			continue
		}
		media += 3000
		if err := w.Append(fr.seq, media, fr.payload); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := w.CloseChunk(); err != nil {
		t.Fatal(err)
	}
	if err := w.Finish(); err != nil {
		t.Fatal(err)
	}
	if err := w.Publish(); err != nil {
		t.Fatal(err)
	}

	rep, err := VerifyFile(out, VerifyOptions{SourcePath: input})
	if err != nil {
		t.Fatal(err)
	}
	if rep.UnindexedSource == 0 {
		t.Fatal("dropped source frames were not counted")
	}
}
