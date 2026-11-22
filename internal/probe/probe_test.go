package probe

import (
	"os"
	"path/filepath"
	"testing"

	"mediacheck/internal/fault"
	"mediacheck/internal/fixture"
)

func generate(t *testing.T, kind fixture.Kind) fixture.Result {
	t.Helper()
	r, err := fixture.Generate(t.TempDir(), fixture.DefaultSpec(kind))
	if err != nil {
		t.Fatalf("generate %s: %v", kind, err)
	}
	return r
}

func analyze(t *testing.T, path string) *File {
	t.Helper()
	f, err := Analyze(path, DefaultConfig())
	if err != nil {
		t.Fatalf("analyze %s: %v", path, err)
	}
	return f
}

func trackByKind(t *testing.T, f *File, kind Kind) *Track {
	t.Helper()
	for _, tr := range f.Tracks {
		if tr.Kind == kind {
			return tr
		}
	}
	t.Fatalf("no %s track", kind)
	return nil
}

func TestValidStructure(t *testing.T) {
	r := generate(t, fixture.Valid)
	f := analyze(t, r.Path)
	if f.Truncated {
		t.Fatal("valid file reported truncated")
	}
	if f.FirstUnreadable != -1 {
		t.Fatalf("FirstUnreadable=%d want -1", f.FirstUnreadable)
	}
	if len(f.Tracks) != 2 {
		t.Fatalf("tracks=%d want 2", len(f.Tracks))
	}
	v := trackByKind(t, f, KindVideo)
	if v.Codec != "h264" || v.Timescale != fixture.VideoTimescale {
		t.Fatalf("video codec=%s ts=%d", v.Codec, v.Timescale)
	}
	if v.Declared != 90 || v.Walked != 90 {
		t.Fatalf("video declared=%d walked=%d", v.Declared, v.Walked)
	}
	if v.Keyframes != 3 {
		t.Fatalf("keyframes=%d want 3", v.Keyframes)
	}
	if v.MaxKeyframeGapMS != 1000 || v.MinKeyframeGapMS != 1000 || v.MeanKeyframeGapMS != 1000 {
		t.Fatalf("gaps max/mean/min=%d/%d/%d", v.MaxKeyframeGapMS, v.MeanKeyframeGapMS, v.MinKeyframeGapMS)
	}
	if !v.Index.Complete || !v.Index.SyncSamples {
		t.Fatalf("video index coverage=%+v", v.Index)
	}
	a := trackByKind(t, f, KindAudio)
	if a.Codec != "aac" || a.Walked != 135 {
		t.Fatalf("audio codec=%s walked=%d", a.Codec, a.Walked)
	}
	if uint64(a.Keyframes) != a.Walked {
		t.Fatalf("audio samples should all be sync: %d/%d", a.Keyframes, a.Walked)
	}
	if len(f.TrackIssues()) != 0 {
		t.Fatalf("unexpected issues: %v", f.TrackIssues())
	}
}

func TestTruncationLocatesTail(t *testing.T) {
	r := generate(t, fixture.Truncated)
	f := analyze(t, r.Path)
	if !f.Truncated {
		t.Fatal("truncated file not flagged")
	}
	if f.FirstUnreadable < 0 {
		t.Fatal("FirstUnreadable not set")
	}
	if f.DiscardedBytes <= 0 {
		t.Fatalf("DiscardedBytes=%d", f.DiscardedBytes)
	}
	st, err := os.Stat(r.Path)
	if err != nil {
		t.Fatal(err)
	}
	if f.FirstUnreadable > st.Size() {
		t.Fatalf("FirstUnreadable %d past file %d", f.FirstUnreadable, st.Size())
	}
	var trunc int64 = -1
	for _, is := range f.TrackIssues() {
		if is.Code == fault.CodeTruncated && (trunc < 0 || is.Offset < trunc) {
			trunc = is.Offset
		}
	}
	if trunc < 0 {
		t.Fatal("no truncated fault reported")
	}
}

func TestCorruptInputKeepsStructureReadable(t *testing.T) {
	r := generate(t, fixture.Corrupt)
	f := analyze(t, r.Path)
	if f.Truncated {
		t.Fatal("corrupt file must not be reported as truncated")
	}
	for _, tr := range f.Tracks {
		if tr.UnreadableOffset >= 0 {
			t.Fatalf("track %d unexpectedly unreadable at %d", tr.ID, tr.UnreadableOffset)
		}
	}
	if r.DamageOffset <= 0 {
		t.Fatalf("fixture damage offset=%d", r.DamageOffset)
	}
}

func TestMissingIndexRebuildsOffsets(t *testing.T) {
	r := generate(t, fixture.MissingIndex)
	f := analyze(t, r.Path)
	v := trackByKind(t, f, KindVideo)
	if v.Index.Complete {
		t.Fatalf("missing-index fixture reported complete index: %+v", v.Index)
	}
	if !v.Index.Rebuilt || !v.Index.ChunkOffsets {
		t.Fatalf("offsets not rebuilt: %+v", v.Index)
	}
	if got, want := v.Walked, uint64(90); got != want {
		t.Fatalf("walked=%d want %d", got, want)
	}
	found := false
	for _, is := range v.Issues {
		if is.Code == fault.CodeIndexMissing {
			found = true
		}
	}
	if !found {
		t.Fatal("index_missing fault not reported")
	}
}

func TestBoundaryMismatchEditList(t *testing.T) {
	r := generate(t, fixture.BoundaryMismatch)
	f := analyze(t, r.Path)
	v := trackByKind(t, f, KindVideo)
	a := trackByKind(t, f, KindAudio)
	if v.StartMS != 0 {
		t.Fatalf("video start=%d", v.StartMS)
	}
	if a.StartMS != 40 {
		t.Fatalf("audio start=%d want 40", a.StartMS)
	}
}

func TestRejectsInvalidInputs(t *testing.T) {
	if _, err := Analyze(filepath.Join(t.TempDir(), "missing.mp4"), DefaultConfig()); err == nil {
		t.Fatal("expected error for missing input")
	}
	if _, err := Analyze(t.TempDir(), DefaultConfig()); err == nil {
		t.Fatal("expected error for directory input")
	}
}

func TestGarbageAndShortInputs(t *testing.T) {
	dir := t.TempDir()
	garbage := filepath.Join(dir, "garbage.mp4")
	if err := os.WriteFile(garbage, []byte("not an mp4 at all"), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := Analyze(garbage, DefaultConfig())
	if err != nil {
		t.Fatalf("garbage should not hard-fail: %v", err)
	}
	if len(f.Tracks) != 0 {
		t.Fatalf("garbage produced tracks: %d", len(f.Tracks))
	}
	short := filepath.Join(dir, "short.mp4")
	if err := os.WriteFile(short, make([]byte, 4), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err = Analyze(short, DefaultConfig())
	if err != nil {
		t.Fatalf("short file should not hard-fail: %v", err)
	}
	if !f.Truncated {
		t.Fatal("short file should be truncated")
	}
}

func TestDeterministicAnalysis(t *testing.T) {
	r := generate(t, fixture.Truncated)
	a := analyze(t, r.Path)
	b := analyze(t, r.Path)
	if a.FirstUnreadable != b.FirstUnreadable || a.DiscardedBytes != b.DiscardedBytes {
		t.Fatal("probe results differ between runs")
	}
	for i := range a.Tracks {
		if a.Tracks[i].Walked != b.Tracks[i].Walked || a.Tracks[i].Keyframes != b.Tracks[i].Keyframes {
			t.Fatalf("track %d differs between runs", i)
		}
	}
}

func TestCursorMatchesDeclaredOrder(t *testing.T) {
	r := generate(t, fixture.Valid)
	f := analyze(t, r.Path)
	v := trackByKind(t, f, KindVideo)
	cur := v.Cursor()
	var prev int64 = -1
	var n uint64
	for {
		row, ok := cur.Next()
		if !ok {
			break
		}
		if prev >= 0 && row.DTSMS < prev {
			t.Fatalf("timestamp regressed at sample %d", row.Index)
		}
		prev = row.DTSMS
		if !row.InBounds || !row.InMdat {
			t.Fatalf("sample %d out of bounds", row.Index)
		}
		n++
	}
	if n != 90 {
		t.Fatalf("cursor yielded %d samples", n)
	}
}
