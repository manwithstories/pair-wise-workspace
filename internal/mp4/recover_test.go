package mp4

import (
	"testing"

	"mediasim/internal/fixture"
)

func TestRecoverMissingIndexFindsEverySample(t *testing.T) {
	path, man := writePreset(t, "missing-index")
	info, err := Parse(path)
	if err != nil {
		t.Fatal(err)
	}
	rec, err := Recover(path, info)
	if err != nil {
		t.Fatal(err)
	}
	if !rec.Resynced {
		t.Fatal("missing-index recovery should report resync")
	}
	perSource := map[string]int{}
	for _, b := range rec.Committing() {
		perSource[b.Source]++
	}
	for _, ts := range man.Tracks {
		if perSource[ts.Source] != ts.Count {
			t.Fatalf("%s recovered %d want %d", ts.Source, perSource[ts.Source], ts.Count)
		}
	}
	// Every boundary is complete (in range) and sized like the manifest.
	for _, b := range rec.Committing() {
		ts, ok := man.Track(b.Source)
		if !ok {
			t.Fatalf("unknown source %q", b.Source)
		}
		if int(b.Size) != ts.SampleSize {
			t.Fatalf("%s[%d] size=%d want %d", b.Source, b.Index, b.Size, ts.SampleSize)
		}
	}
	if len(rec.Skipped) != 0 {
		t.Fatalf("unexpected skipped ranges: %v", rec.Skipped)
	}
}

func TestRecoverDoesNotDuplicateIndexedSamples(t *testing.T) {
	path, _ := writePreset(t, "normal")
	info, err := Parse(path)
	if err != nil {
		t.Fatal(err)
	}
	rec, err := Recover(path, info)
	if err != nil {
		t.Fatal(err)
	}
	known := 0
	for _, tr := range info.Tracks {
		known += tr.ValidCount()
	}
	if len(rec.Committing()) != known {
		t.Fatalf("committed=%d indexed=%d", len(rec.Committing()), known)
	}
	for _, b := range rec.Committing() {
		if !b.InStbl {
			t.Fatalf("boundary %s[%d] not linked to its sample", b.Source, b.Index)
		}
	}
}

func TestRecoverAfterTruncationStopsAtEnd(t *testing.T) {
	path, _ := writePreset(t, "truncated")
	info, err := Parse(path)
	if err != nil {
		t.Fatal(err)
	}
	rec, err := Recover(path, info)
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.Committing()) == 0 {
		t.Fatal("no boundaries recovered")
	}
	// The recovered boundary rows must all be complete samples, never a
	// partial trailing sample.
	for _, b := range rec.Committing() {
		if b.Offset+int64(b.Size) > info.Size {
			t.Fatalf("boundary %+v extends past EOF %d", b, info.Size)
		}
	}
}

func TestRecoverCorruptRunSkipsBadSample(t *testing.T) {
	path, _ := writePreset(t, "corrupt")
	info, err := Parse(path)
	if err != nil {
		t.Fatal(err)
	}
	rec, err := Recover(path, info)
	if err != nil {
		t.Fatal(err)
	}
	byIdx := map[int]bool{}
	for _, b := range rec.Committing() {
		if b.Source == fixture.SourceCam0 {
			byIdx[b.Index] = true
		}
	}
	// The scanner reacts to the corrupt magic: sample 30's header is gone.
	if byIdx[30] && byIdx[31] && byIdx[32] {
		t.Fatal("corrupt samples were recovered as valid boundaries")
	}
	// Neighbours must remain.
	if !byIdx[29] || !byIdx[33] {
		t.Fatalf("valid neighbours lost: %v", byIdx)
	}
	total, committed, phantoms := rec.Counts()
	if total != committed+phantoms {
		t.Fatalf("counts %d %d %d", total, committed, phantoms)
	}
}

func TestRecoverIgnoresForeignContainer(t *testing.T) {
	path, _ := writePreset(t, "normal")
	info, err := Parse(path)
	if err != nil {
		t.Fatal(err)
	}
	info.Synthetic = false
	rec, err := Recover(path, info)
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.Boundaries) != 0 {
		t.Fatal("foreign media must not be scanned for MSMP headers")
	}
}
