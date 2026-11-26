package checkpoint

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/relayteam/relay/internal/media"
	"github.com/relayteam/relay/internal/timeline"
)

func testRecords(start, n int) []timeline.Record {
	out := make([]timeline.Record, n)
	for i := range out {
		no := start + i
		r := timeline.Record{
			Track: "main", Rendition: "720p", Seq: int64(no),
			PTS90k: int64(no) * 3000, Dur90k: 3000, Clip: "c",
			Payload: "checkpoint-payload",
		}
		r.ComputeCRC()
		r.RecordNo = no + 1
		r.LineNo = no + 1
		out[i] = r
	}
	return out
}

func testConfig() Config {
	return Config{InputName: "in.ndjson", ChunkRecords: 3, MaxInflightBytes: 4096}
}

func TestRestoreVerifiesChunksAndRemovesOrphans(t *testing.T) {
	root := t.TempDir()
	cfg := testConfig()
	m, err := NewManager(root, "input-fp", cfg)
	if err != nil {
		t.Fatal(err)
	}
	batch := testRecords(0, 3)
	if _, err := m.CommitChunk(batch); err != nil {
		t.Fatal(err)
	}
	if m.Index.NextRecordNo != 4 {
		t.Fatalf("next=%d, want 4", m.Index.NextRecordNo)
	}

	orphan := filepath.Join(root, chunkDir, "chunk-999999.ndjson")
	if err := os.WriteFile(orphan, []byte(`{"orphan":true}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	rm, restoreMS, err := Restore(root, "input-fp", cfg)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if restoreMS > 5000 {
		t.Fatalf("restore_ms=%d, want <5000", restoreMS)
	}
	if rm.Index.NextRecordNo != 4 || len(rm.Index.Chunks) != 1 {
		t.Fatalf("restored index=%+v", rm.Index)
	}
	if _, err := os.Stat(orphan); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("orphan should be removed, stat err=%v", err)
	}
	count := 0
	if err := rm.StreamCommitted(func(r timeline.Record) error {
		if r.CRC32 == 0 {
			t.Fatal("replayed record missing CRC")
		}
		count++
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if count != 3 {
		t.Fatalf("replayed=%d, want 3", count)
	}
}

func TestRestoreRejectsTamperedCommittedChunk(t *testing.T) {
	root := t.TempDir()
	cfg := testConfig()
	m, err := NewManager(root, "fp", cfg)
	if err != nil {
		t.Fatal(err)
	}
	c, err := m.CommitChunk(testRecords(0, 2))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, c.File)
	if err := os.WriteFile(path, []byte(`{"tampered":true}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, err = Restore(root, "fp", cfg)
	if !errors.Is(err, media.ErrChunkDigest) {
		t.Fatalf("err=%v, want chunk digest error", err)
	}
}

func TestRestoreRejectsConfigAndInputChanges(t *testing.T) {
	root := t.TempDir()
	cfg := testConfig()
	m, err := NewManager(root, "fp", cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.CommitChunk(testRecords(0, 1)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Restore(root, "different-input", cfg); !errors.Is(err, media.ErrInputFingerprint) {
		t.Fatalf("input mismatch err=%v", err)
	}
	changed := cfg
	changed.ChunkRecords = 999
	if _, _, err := Restore(root, "fp", changed); !errors.Is(err, media.ErrConfigFingerprint) {
		t.Fatalf("config mismatch err=%v", err)
	}
}
