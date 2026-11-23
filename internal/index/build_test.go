package index_test

import (
	"os"
	"path/filepath"
	"testing"

	"mediad/internal/index"
	"mediad/internal/ingest"
	"mediad/internal/model"
	"mediad/internal/synth"
)

// opened is a source file plus its scanner output, ready for Build.
type opened struct {
	f      *os.File
	frames []model.Frame
	path   string
}

func (o *opened) TrackID() uint32                           { return 1 }
func (o *opened) Read(p []byte) (int, error)                { return o.f.Read(p) }
func (o *opened) Seek(off int64, whence int) (int64, error) { return o.f.Seek(off, whence) }
func (o *opened) FrameAt(offset int64, size uint64) (*ingest.FrameBox, error) {
	return ingest.ReadFrameAt(o.f, offset, size)
}

func prepare(t *testing.T, spec synth.TrackSpec) (*opened, string) {
	t.Helper()
	dir := t.TempDir()
	src := filepath.Join(dir, "track.mediad")
	f, err := os.Create(src)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := synth.Generate(f, spec, 7); err != nil {
		t.Fatalf("generate: %v", err)
	}
	if _, err := f.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	var frames []model.Frame
	if _, err := ingest.Scan(f, func(fr model.Frame) error {
		frames = append(frames, fr)
		return nil
	}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	return &opened{f: f, frames: frames, path: src}, filepath.Join(dir, "track.idx")
}

func TestBuildIndexesOnlyUsableFrames(t *testing.T) {
	spec := synth.DefaultSpec(1, 90)
	spec.CorruptFrames = 2
	spec.TruncateTail = true
	src, idxPath := prepare(t, spec)
	defer src.f.Close()

	st, err := index.Build(src, idxPath, src.frames)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	usable := 0
	for _, fr := range src.frames {
		if fr.Status.Usable() {
			usable++
		}
	}
	if st.Entries != usable {
		t.Fatalf("entries = %d, want %d", st.Entries, usable)
	}
	if st.Skipped < 3 {
		t.Fatalf("skipped = %d, want >= 3 (2 corrupt + 1 tail)", st.Skipped)
	}
	entries, dropped, err := index.Open(idxPath)
	if err != nil || dropped != 0 {
		t.Fatalf("open: err=%v dropped=%d", err, dropped)
	}
	for _, e := range entries {
		if e.FrameNo == uint64(spec.Frames+1) {
			t.Fatalf("truncated frame %d entered the index", e.FrameNo)
		}
		// Every entry must point at a valid, usable frame with matching PTS.
		fr, ok := findFrame(src.frames, e.Offset)
		if !ok || !fr.Status.Usable() {
			t.Fatalf("entry frame %d points at a non-usable frame", e.FrameNo)
		}
		if fr.PTS != e.PTS {
			t.Fatalf("entry %d pts %d != frame pts %d", e.FrameNo, e.PTS, fr.PTS)
		}
	}
}

func TestBuildReplacesIncompleteOldIndex(t *testing.T) {
	src, idxPath := prepare(t, synth.DefaultSpec(2, 50))
	defer src.f.Close()
	if _, err := index.Build(src, idxPath, src.frames); err != nil {
		t.Fatalf("first build: %v", err)
	}
	// Simulate an interrupted rebuild: chop the file mid-entry.
	good, err := os.ReadFile(idxPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(idxPath, good[:len(good)-17], 0o644); err != nil {
		t.Fatal(err)
	}
	if _, dropped, err := index.Open(idxPath); err != nil || dropped == 0 {
		t.Fatalf("truncated index must report dropped entries: err=%v dropped=%d", err, dropped)
	}
	st, err := index.Build(src, idxPath, src.frames)
	if err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	if !st.Replaced || st.Truncated == 0 {
		t.Fatalf("stats %+v: want replaced with truncated entries", st)
	}
	if _, dropped, err := index.Open(idxPath); err != nil || dropped != 0 {
		t.Fatalf("replaced index still partial: err=%v dropped=%d", err, dropped)
	}
}

func TestBuildIsIdempotent(t *testing.T) {
	src, idxPath := prepare(t, synth.DefaultSpec(3, 40))
	defer src.f.Close()
	if _, err := index.Build(src, idxPath, src.frames); err != nil {
		t.Fatal(err)
	}
	a, _ := os.ReadFile(idxPath)
	if _, err := index.Build(src, idxPath, src.frames); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(idxPath)
	if string(a) != string(b) {
		t.Fatal("rebuild produced a different index for identical input")
	}
}

func TestValidateRejectsDriftedEntry(t *testing.T) {
	src, idxPath := prepare(t, synth.DefaultSpec(4, 10))
	defer src.f.Close()
	if _, err := index.Build(src, idxPath, src.frames); err != nil {
		t.Fatal(err)
	}
	entries, _, err := index.Open(idxPath)
	if err != nil {
		t.Fatal(err)
	}
	// Corrupt one entry's PTS but keep its CRC valid by re-encoding: a stale
	// index must fail validation against the source.
	bad := entries
	bad[0].PTS++
	err = index.Validate(bad, func(e index.Entry) ([32]byte, int64, error) {
		fb, err := ingest.ReadFrameAt(src.f, e.Offset, uint64(e.Size))
		if err != nil {
			return [32]byte{}, 0, err
		}
		// Return the true mapped PTS from the scan record.
		fr, _ := findFrame(src.frames, e.Offset)
		return model.DigestBytes(fb.Payload), fr.PTS, nil
	})
	if err == nil {
		t.Fatal("drifted entry passed validation")
	}
}

func TestLookupFindsIndexedFrameAndMissesBad(t *testing.T) {
	spec := synth.DefaultSpec(5, 30)
	spec.CorruptFrames = 1
	src, idxPath := prepare(t, spec)
	defer src.f.Close()
	if _, err := index.Build(src, idxPath, src.frames); err != nil {
		t.Fatal(err)
	}
	entries, _, err := index.Open(idxPath)
	if err != nil {
		t.Fatal(err)
	}
	var corrupted uint64
	for _, fr := range src.frames {
		if fr.Status != model.FrameOK {
			corrupted = fr.Meta.FrameNo
		}
	}
	if _, ok := index.Lookup(entries, corrupted); ok {
		t.Fatalf("corrupt frame %d is in the index", corrupted)
	}
	if e, ok := index.Lookup(entries, 1); !ok || e.FrameNo != 1 {
		t.Fatalf("frame 1 not found: %+v %v", e, ok)
	}
	if _, ok := index.Lookup(entries, 9999); ok {
		t.Fatal("lookup of nonexistent frame succeeded")
	}
}

func findFrame(frames []model.Frame, offset int64) (model.Frame, bool) {
	for _, fr := range frames {
		if fr.Offset == offset {
			return fr, true
		}
	}
	return model.Frame{}, false
}
