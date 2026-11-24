package ingest_test

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"

	"mediad/internal/ingest"
	"mediad/internal/model"
	"mediad/internal/synth"
)

func generate(t *testing.T, spec synth.TrackSpec) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "track.mediad")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := synth.Generate(f, spec, 42); err != nil {
		t.Fatalf("generate: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func scanFile(t *testing.T, path string) (*ingest.Summary, []model.Frame) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var frames []model.Frame
	sum, err := ingest.Scan(f, func(fr model.Frame) error {
		frames = append(frames, fr)
		return nil
	})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	return sum, frames
}

func TestScanHealthyTrack(t *testing.T) {
	spec := synth.DefaultSpec(1, 100)
	sum, frames := scanFile(t, generate(t, spec))
	if sum.Track.ID != 1 {
		t.Fatalf("track id = %d", sum.Track.ID)
	}
	if len(frames) != 100 || sum.Frames != 100 {
		t.Fatalf("frames = %d/%d, want 100", len(frames), sum.Frames)
	}
	var prev int64 = -1
	for i, fr := range frames {
		if fr.Status != model.FrameOK {
			t.Fatalf("frame %d status = %s (%s)", i, fr.Status, fr.Detail)
		}
		if fr.PTS <= prev {
			t.Fatalf("frame %d: PTS %d not monotonic after %d", i, fr.PTS, prev)
		}
		prev = fr.PTS
		if fr.Meta.FrameNo != uint64(i+1) {
			t.Fatalf("frame %d: number %d", i, fr.Meta.FrameNo)
		}
	}
	if sum.HashBad+sum.ImageBad+sum.Truncated != 0 {
		t.Fatalf("unexpected defects %+v", sum)
	}
}

func TestScanIsolatesCorruptFrames(t *testing.T) {
	spec := synth.DefaultSpec(2, 120)
	spec.CorruptFrames = 3
	sum, frames := scanFile(t, generate(t, spec))
	bad := 0
	for _, fr := range frames {
		if fr.Status == model.FrameHashMismatch {
			bad++
		}
	}
	if bad != 3 {
		t.Fatalf("hash-bad frames = %d, want 3", bad)
	}
	if sum.HashBad != 3 {
		t.Fatalf("summary hash bad = %d", sum.HashBad)
	}
	// A single corrupt frame must not abort the scan: all 120 frames seen.
	if len(frames) != 120 {
		t.Fatalf("frames seen = %d, want 120", len(frames))
	}
}

func TestScanMarksTruncatedTail(t *testing.T) {
	spec := synth.DefaultSpec(3, 60)
	spec.TruncateTail = true
	sum, frames := scanFile(t, generate(t, spec))
	if sum.Truncated != 1 {
		t.Fatalf("truncated = %d, want 1", sum.Truncated)
	}
	last := frames[len(frames)-1]
	if last.Status != model.FrameTruncatedTail {
		t.Fatalf("last frame status = %s, want truncated_tail", last.Status)
	}
	if last.Meta.FrameNo != 61 {
		t.Fatalf("torn frame number = %d, want 61", last.Meta.FrameNo)
	}
	if last.Status.Usable() {
		t.Fatal("truncated frame must not be usable")
	}
}

func TestScanDetectsConflictingFrameNumbers(t *testing.T) {
	spec := synth.DefaultSpec(4, 80)
	spec.ConflictEvery = 10
	sum, frames := scanFile(t, generate(t, spec))
	if sum.Conflicts == 0 {
		t.Fatal("no conflicts detected")
	}
	for _, fr := range frames {
		if fr.Status == model.FrameConflict && fr.Status.Usable() {
			t.Fatal("conflict frame must not be usable")
		}
	}
}

func TestScanDetectsDuplicates(t *testing.T) {
	spec := synth.DefaultSpec(5, 40)
	spec.DuplicateEvery = 7
	sum, frames := scanFile(t, generate(t, spec))
	if sum.Dup == 0 {
		t.Fatal("no duplicates detected")
	}
	// Duplicates are still delivered (so counts are honest) but unusable.
	for _, fr := range frames {
		if fr.Status == model.FrameDuplicate && fr.Status.Usable() {
			t.Fatal("duplicate frame must not be usable")
		}
	}
}

func TestScanRejectsMissingTrack(t *testing.T) {
	var buf bytes.Buffer
	f := newMemFile(&buf)
	// Write only a file-type box.
	if err := ingest.WriteFtyp(f); err != nil {
		t.Fatal(err)
	}
	if _, err := ingest.Scan(f, func(model.Frame) error { return nil }); err == nil {
		t.Fatal("expected fatal error for missing track")
	} else if !model.IsClass(err, model.ClassFatal) {
		t.Fatalf("class = %v, want fatal", model.ClassOf(err))
	}
}

func TestTruncatedContainerHeadIsFatal(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cut.mediad")
	full := generate(t, synth.DefaultSpec(6, 20))
	data, err := os.ReadFile(full)
	if err != nil {
		t.Fatal(err)
	}
	// Keep only the first 12 bytes: a partial box header, no track box.
	if err := os.WriteFile(path, data[:12], 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := ingest.Scan(f, func(model.Frame) error { return nil }); err == nil {
		t.Fatal("expected fatal error")
	} else if model.ClassOf(err) != model.ClassFatal {
		t.Fatalf("class = %v, want fatal", model.ClassOf(err))
	}
}

func TestConflictingFramesDoNotBreakIndexedMonotonicity(t *testing.T) {
	spec := synth.DefaultSpec(1, 100)
	spec.ConflictEvery = 10
	path := generate(t, spec)
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	cur, err := ingest.NewCursor(f, nil)
	if err != nil {
		t.Fatal(err)
	}
	var prevOK int64 = -1
	conflicts := 0
	for {
		fr, err := cur.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if fr.Status == model.FrameConflict {
			conflicts++
			continue
		}
		if fr.Status == model.FrameOK {
			if fr.PTS < prevOK {
				t.Fatalf("indexed frame %d pts %d went back past %d", fr.Meta.FrameNo, fr.PTS, prevOK)
			}
			prevOK = fr.PTS
		}
	}
	if conflicts == 0 {
		t.Fatal("expected conflicting frames")
	}
}
