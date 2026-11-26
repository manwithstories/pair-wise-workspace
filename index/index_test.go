package index_test

import (
	"os"
	"path/filepath"
	"testing"

	"archivectl/internal/core"
	"archivectl/internal/fixture"
	"archivectl/internal/index"
	iwav "archivectl/internal/wavscan"
)

func TestIndexAtomicSaveLoadAndInputInvalidation(t *testing.T) {
	dir := t.TempDir()
	m, err := fixture.Generate(fixture.Config{
		Dir: dir, Seed: 11, Frames: 16384, Channels: 2, Bits: 16,
		Injections:     []string{"wrap", "drift", "vfr", "bad-crc", "truncate", "missing-cue"},
		TruncateFrames: 4096,
	})
	if err != nil {
		t.Fatal(err)
	}
	rep, err := iwav.ScanPath(m.DamagedWAV)
	if err != nil {
		t.Fatal(err)
	}
	cf, err := fixture.LoadClockFile(m.ClockJSON)
	if err != nil {
		t.Fatal(err)
	}
	idx, err := index.Build(index.BuildInput{
		WAVPath: m.DamagedWAV, ClockPath: m.ClockJSON, ChecksPath: m.ChecksJSON,
		Report: rep, BlockRecords: cf.Blocks, Damage: m.Damage,
	})
	if err != nil {
		t.Fatal(err)
	}
	if idx.IndexVersion != index.Version || idx.WAVSHA256 == "" || idx.ClockSHA256 == "" {
		t.Fatalf("index key incomplete: %+v", idx.Key)
	}
	path := filepath.Join(dir, "nested", "archive-index.json")
	if err := idx.SaveAtomic(path); err != nil {
		t.Fatal(err)
	}
	loaded, err := index.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := loaded.ValidateInputs(m.DamagedWAV, m.ClockJSON); err != nil {
		t.Fatalf("fresh index should validate: %v", err)
	}

	appendFile(t, m.ClockJSON, []byte("\n// changed\n"))
	err = loaded.ValidateInputs(m.DamagedWAV, m.ClockJSON)
	if !core.AsKind(err, core.KindIndexStale) {
		t.Fatalf("clock change should stale index, got %v", err)
	}

	appendFile(t, m.DamagedWAV, []byte("concurrent-write"))
	err = loaded.ValidateInputs(m.DamagedWAV, m.ClockJSON)
	if !core.AsKind(err, core.KindIndexStale) {
		t.Fatalf("WAV change should stale index, got %v", err)
	}

	loaded.IndexVersion = "archive-index/v0"
	if err := loaded.ValidateInputs(m.DamagedWAV, m.ClockJSON); !core.AsKind(err, core.KindIndexStale) {
		t.Fatalf("version change should stale index, got %v", err)
	}
}

func appendFile(t *testing.T, path string, b []byte) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.Write(b); err != nil {
		t.Fatal(err)
	}
}
