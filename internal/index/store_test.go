package index

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"mediaroute/internal/clock"
)

func sampleIndex(t *testing.T, marker string) *Index {
	t.Helper()
	ix := &Index{
		SchemaVersion:  SchemaVersion,
		ScannerVersion: "scan-v1",
		ManifestDigest: "manifest-" + marker,
		InputDigest:    "input-" + marker,
		Tracks: []Track{{
			ID:        1,
			Kind:      "video",
			Source:    "v.mp4",
			Timescale: 90000,
			Origin:    clock.Rational{Num: 111060, Den: 90000},
			Samples: []Sample{
				{Number: 1, DTS: 0, CTS: 0, Duration: 3000, Offset: 100, Size: 40, Sync: true, Readable: true},
				{Number: 2, DTS: 3000, CTS: 3000, Duration: 3000, Offset: 140, Size: 40, Sync: false, Readable: true},
			},
			Keyframes: []int64{1},
			Entry:     SampleEntry{Type: "avc1", Width: 320, Height: 240},
		}},
	}
	if err := ix.Finalize(); err != nil {
		t.Fatal(err)
	}
	return ix
}

func TestSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s := &Store{Dir: dir}
	ix := sampleIndex(t, "a")
	k := Key{InputDigest: ix.InputDigest, ScannerVersion: ix.ScannerVersion, ManifestDigest: ix.ManifestDigest}
	if err := s.Save(k, ix); err != nil {
		t.Fatal(err)
	}
	got, hit, err := s.Load(k)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !hit {
		t.Fatal("want cache hit")
	}
	if !Equal(got, ix) {
		t.Fatalf("summary mismatch: %s vs %s", got.Summary, ix.Summary)
	}
	if got.IndexedSamples != 2 {
		t.Fatalf("want 2 indexed samples got %d", got.IndexedSamples)
	}
}

func TestLoadMissOnChangedInputDigest(t *testing.T) {
	dir := t.TempDir()
	s := &Store{Dir: dir}
	a := sampleIndex(t, "a")
	ka := Key{InputDigest: a.InputDigest, ScannerVersion: a.ScannerVersion, ManifestDigest: a.ManifestDigest}
	if err := s.Save(ka, a); err != nil {
		t.Fatal(err)
	}
	b := sampleIndex(t, "b")
	kb := Key{InputDigest: b.InputDigest, ScannerVersion: b.ScannerVersion, ManifestDigest: b.ManifestDigest}
	got, hit, err := s.Load(kb)
	if err != nil || hit || got != nil {
		t.Fatalf("changed media must miss: hit=%v got=%v err=%v", hit, got, err)
	}
}

func TestCorruptCacheIsRebuilt(t *testing.T) {
	dir := t.TempDir()
	s := &Store{Dir: dir}
	ix := sampleIndex(t, "a")
	k := Key{InputDigest: ix.InputDigest, ScannerVersion: ix.ScannerVersion, ManifestDigest: ix.ManifestDigest}
	if err := s.Save(k, ix); err != nil {
		t.Fatal(err)
	}
	// Corrupt the payload: flip a sample offset in the JSON.
	path := s.Path(k)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var env map[string]any
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	idx := env["index"].(map[string]any)
	tracks := idx["tracks"].([]any)
	tr := tracks[0].(map[string]any)
	samples := tr["samples"].([]any)
	s0 := samples[0].(map[string]any)
	s0["offset"] = float64(999999)
	raw, _ = json.Marshal(env)
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	_, hit, err := s.Load(k)
	if err == nil || hit {
		t.Fatalf("corrupt cache must be reported: hit=%v err=%v", hit, err)
	}
	// Safe rebuild: overwrite with good data and reload.
	if err := s.Save(k, ix); err != nil {
		t.Fatal(err)
	}
	if _, hit, err := s.Load(k); err != nil || !hit {
		t.Fatalf("rebuild failed: hit=%v err=%v", hit, err)
	}
}

func TestAtomicWriteLeavesNoTempFiles(t *testing.T) {
	dir := t.TempDir()
	s := &Store{Dir: dir}
	ix := sampleIndex(t, "a")
	k := Key{InputDigest: ix.InputDigest, ScannerVersion: "scan-v1", ManifestDigest: ix.ManifestDigest}
	if err := s.Save(k, ix); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".tmp" {
			t.Fatalf("temp file left behind: %s", e.Name())
		}
	}
}

func TestOversizedCacheRejected(t *testing.T) {
	dir := t.TempDir()
	s := &Store{Dir: dir, MaxBytes: 32}
	ix := sampleIndex(t, "a")
	k := Key{InputDigest: ix.InputDigest, ScannerVersion: ix.ScannerVersion, ManifestDigest: ix.ManifestDigest}
	if err := s.Save(k, ix); err == nil {
		t.Fatal("want oversize error")
	}
}

func TestSummaryStableAcrossRepeatedScans(t *testing.T) {
	a := sampleIndex(t, "a")
	b := sampleIndex(t, "a")
	if a.Summary != b.Summary {
		t.Fatalf("summaries must match: %s vs %s", a.Summary, b.Summary)
	}
}
