package index

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"mediaaudit/internal/model"
)

func testCfg() Config {
	return Config{
		PackagePath:      "/tmp/pkg",
		ChunkFrames:      4096,
		CheckpointEvery:  8,
		TileFrames:       32,
		InputFingerprint: "deadbeef",
	}
}

func TestCreateLoadAndAppend(t *testing.T) {
	p := filepath.Join(t.TempDir(), "idx.pfi")
	f, err := Create(p, testCfg(), 480000)
	if err != nil {
		t.Fatal(err)
	}
	for seq := int64(0); seq < 5; seq++ {
		if err := f.AppendChunk(ChunkRecord{
			Seq: seq, FirstFrame: seq * 4096, LastFrame: (seq + 1) * 4096,
			Status: "ok",
		}); err != nil {
			t.Fatal(err)
		}
	}
	if f.NextSeq() != 5 {
		t.Fatalf("next seq=%d want 5", f.NextSeq())
	}
	// Reload from disk and verify checksum round-trips.
	g, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if g.SHA256() != f.SHA256() {
		t.Fatal("checksum changed on reload")
	}
	if g.Payload.CommittedSeq != 4 || len(g.Payload.Chunks) != 5 {
		t.Fatalf("reloaded payload wrong: %+v", g.Payload)
	}
}

func TestOutOfOrderAppendRejected(t *testing.T) {
	p := filepath.Join(t.TempDir(), "idx.pfi")
	f, _ := Create(p, testCfg(), 100)
	err := f.AppendChunk(ChunkRecord{Seq: 3, FirstFrame: 0, LastFrame: 10, Status: "ok"})
	if err == nil || !errors.Is(err, model.ErrIndexUnrecoverable) {
		t.Fatalf("expected index fault, got %v", err)
	}
}

func TestConfigMismatchRejectsResume(t *testing.T) {
	p := filepath.Join(t.TempDir(), "idx.pfi")
	f, _ := Create(p, testCfg(), 100)
	changed := testCfg()
	changed.ChunkFrames = 2048
	if err := f.ValidateConfig(changed); err == nil {
		t.Fatal("expected config mismatch rejection")
	}
	changed = testCfg()
	changed.InputFingerprint = "other"
	if err := f.ValidateConfig(changed); err == nil {
		t.Fatal("expected fingerprint mismatch rejection")
	}
	if err := f.ValidateConfig(testCfg()); err != nil {
		t.Fatalf("matching config should validate: %v", err)
	}
}

func TestTornIndexRejectedThenRollback(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "idx.pfi")
	f, _ := Create(p, testCfg(), 1000)
	for seq := int64(0); seq < 10; seq++ {
		_ = f.AppendChunk(ChunkRecord{
			Seq: seq, FirstFrame: seq * 4096, LastFrame: (seq + 1) * 4096, Status: "ok",
		})
	}
	goodSize := fileSize(t, p)
	// Tear the file to half its size to simulate a crashed partial write.
	half := headerSize + (goodSize-headerSize)/2
	if err := Tear(p, half); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); err == nil {
		t.Fatal("torn index must fail to load")
	}
	// A healthy prior checkpoint must still exist for rollback. In production
	// the runner keeps the last good file; here we rebuild and roll back.
	good, _ := Create(filepath.Join(dir, "rebuilt.pfi"), testCfg(), 1000)
	for seq := int64(0); seq < 10; seq++ {
		_ = good.AppendChunk(ChunkRecord{
			Seq: seq, FirstFrame: seq * 4096, LastFrame: (seq + 1) * 4096, Status: "ok",
		})
	}
	if err := good.Rollback(4); err != nil {
		t.Fatal(err)
	}
	// keep seq 0..4 (5 chunks), so the next chunk to process is seq 5.
	if good.NextSeq() != 5 {
		t.Fatalf("after rollback next seq=%d want 5", good.NextSeq())
	}
	if len(good.Payload.Chunks) != 5 {
		t.Fatalf("chunks after rollback=%d want 5", len(good.Payload.Chunks))
	}
}

func TestDeterministicChecksum(t *testing.T) {
	cfg := testCfg()
	build := func(path string) string {
		f, _ := Create(path, cfg, 100)
		for seq := int64(0); seq < 3; seq++ {
			_ = f.AppendChunk(ChunkRecord{
				Seq: seq, FirstFrame: seq * 4096, LastFrame: (seq + 1) * 4096,
				Status: "ok", DamageCount: 0,
			})
		}
		return f.SHA256()
	}
	a := build(filepath.Join(t.TempDir(), "a.pfi"))
	b := build(filepath.Join(t.TempDir(), "b.pfi"))
	if a != b {
		t.Fatalf("identical operations gave different checksums %s vs %s", a, b)
	}
}

func fileSize(t *testing.T, path string) int {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return int(st.Size())
}
