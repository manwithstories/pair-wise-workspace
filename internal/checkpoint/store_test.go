package checkpoint

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mediq/internal/index"
	"mediq/internal/model"
	"mediq/internal/timeline"
)

func sampleState() *State {
	return &State{
		InputPath:       "/data/a.seq",
		InputSize:       1000,
		InputCRC:        0xdeadbeef,
		CommittedOffset: 512,
		ChunkHead:       ChunkRecord{Index: 1, FirstFrame: 5, LastFrame: 8, Samples: 4, Checksum: 0x22},
		Index:           index.State{Samples: 8, Chunks: 2, PayloadLen: 128, LastMedia: 21000, FirstMedia: 3000},
		Timeline:        timeline.Snapshot{Started: true, NextSeq: 9, HaveMedia: true, LastMedia: 21000},
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s := New(filepath.Join(dir, "job.ckpt"))
	if err := s.Save(sampleState()); err != nil {
		t.Fatal(err)
	}
	st, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if st.Generation != 1 {
		t.Fatalf("generation %d", st.Generation)
	}
	if st.InputCRC != 0xdeadbeef || st.CommittedOffset != 512 || st.ChunkHead.Index != 1 {
		t.Fatalf("%+v", st)
	}
	if st.Timeline.NextSeq != 9 || st.Index.LastMedia != 21000 {
		t.Fatalf("%+v", st)
	}
}

func TestGenerationIncrements(t *testing.T) {
	dir := t.TempDir()
	s := New(filepath.Join(dir, "job.ckpt"))
	st := sampleState()
	for want := uint64(1); want <= 3; want++ {
		if err := s.Save(st); err != nil {
			t.Fatal(err)
		}
		got, err := s.Load()
		if err != nil {
			t.Fatal(err)
		}
		if got.Generation != want {
			t.Fatalf("generation %d want %d", got.Generation, want)
		}
	}
	// A previous checkpoint is kept for diagnosis.
	if _, err := os.Stat(s.backupPath()); err != nil {
		t.Fatalf("no backup: %v", err)
	}
}

func TestLoadMissingIsNotAnError(t *testing.T) {
	s := New(filepath.Join(t.TempDir(), "absent.ckpt"))
	st, err := s.Load()
	if err != nil || st != nil {
		t.Fatalf("%v %v", st, err)
	}
}

func TestTruncatedCheckpointRefused(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "job.ckpt")
	s := New(path)
	if err := s.Save(sampleState()); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw[:len(raw)/2], 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load(); !errors.Is(err, model.ErrCheckpointMismatch) {
		t.Fatalf("got %v", err)
	}
}

func TestBitFlipRefused(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "job.ckpt")
	s := New(path)
	if err := s.Save(sampleState()); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	raw[len(raw)-40] ^= 0x01
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load(); !errors.Is(err, model.ErrCheckpointMismatch) {
		t.Fatalf("got %v", err)
	}
}

func TestStaleGenerationHeaderRefused(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "job.ckpt")
	s := New(path)
	if err := s.Save(sampleState()); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	// Corrupt the generation inside the body without touching the header: the
	// body digest check must catch it first.
	raw[len(raw)-45] ^= 0x01
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load(); !errors.Is(err, model.ErrCheckpointMismatch) {
		t.Fatalf("got %v", err)
	}
}

func TestInconsistentChunkTableRefused(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "job.ckpt")
	s := New(path)
	st := sampleState()
	st.Index.Chunks = 99 // no longer matches the recorded chunk head
	if err := s.Save(st); err != nil {
		t.Fatal(err)
	}
	// Save persists what it was given; Load is where consistency is enforced.
	if _, err := s.Load(); !errors.Is(err, model.ErrCheckpointMismatch) {
		t.Fatalf("got %v", err)
	}
}

func TestValidForInput(t *testing.T) {
	st := sampleState()
	if err := st.ValidForInput("/data/a.seq", 1000, 0xdeadbeef); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		path string
		size int64
		crc  uint32
	}{
		{"/data/b.seq", 1000, 0xdeadbeef},
		{"/data/a.seq", 1001, 0xdeadbeef},
		{"/data/a.seq", 1000, 0xdeadbeee},
	}
	for _, c := range cases {
		if err := st.ValidForInput(c.path, c.size, c.crc); err == nil {
			t.Fatalf("accepted %+v", c)
		}
	}
}

func TestSaveIsAtomicAgainstCrash(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "job.ckpt")
	s := New(path)
	first := sampleState()
	if err := s.Save(first); err != nil {
		t.Fatal(err)
	}
	// Simulate a crash mid-write: a part file exists but the published file is
	// the earlier generation.
	if err := os.WriteFile(s.partPath(), []byte("torn"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got.Generation != 1 || got.InputCRC != first.InputCRC {
		t.Fatalf("%+v", got)
	}
	self, _ := s.Load()
	if self == nil {
		t.Fatal("lost the checkpoint")
	}
	// A later Save replaces the torn part cleanly.
	if err := s.Save(self); err != nil {
		t.Fatal(err)
	}
	reloaded, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Generation != 2 {
		t.Fatalf("generation %d", reloaded.Generation)
	}
}

func TestResetRemovesArtefacts(t *testing.T) {
	dir := t.TempDir()
	s := New(filepath.Join(dir, "job.ckpt"))
	if err := s.Save(sampleState()); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(sampleState()); err != nil {
		t.Fatal(err)
	}
	if err := s.Reset(); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{s.path, s.partPath(), s.backupPath()} {
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s still exists", p)
		}
	}
}

func TestEncodedHeaderFields(t *testing.T) {
	raw, err := encode(sampleState())
	if err != nil {
		t.Fatal(err)
	}
	if string(raw[:4]) != magic {
		t.Fatalf("magic %q", raw[:4])
	}
	if !strings.HasPrefix(string(raw[headerSize:headerSize+1]), "{") {
		t.Fatal("body is not JSON")
	}
}

func TestChunkLogChain(t *testing.T) {
	dir := t.TempDir()
	log := NewChunkLog(filepath.Join(dir, "job.ckpt"))
	if err := log.Reset(); err != nil {
		t.Fatal(err)
	}
	chain := [32]byte{}
	for i := 0; i < 5; i++ {
		c := ChunkRecord{Index: uint32(i), FirstFrame: uint64(i*4 + 1), LastFrame: uint64(i*4 + 4), Samples: 4, Checksum: uint32(i)}
		var err error
		chain, err = log.Append(c, chain)
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := log.Verify(chain, 5); err != nil {
		t.Fatal(err)
	}
	if n, err := log.ChunkCount(); err != nil || n != 5 {
		t.Fatalf("count %d %v", n, err)
	}
	// A wrong expected digest is rejected.
	bad := chain
	bad[0] ^= 0xFF
	if err := log.Verify(bad, 5); err == nil {
		t.Fatal("accepted a wrong chain digest")
	}
	// A truncated log is rejected even if the count looks plausible.
	if err := os.Truncate(log.Path(), int64(chunkRecordSize*4+10)); err != nil {
		t.Fatal(err)
	}
	if err := log.Verify(chain, 5); err == nil {
		t.Fatal("accepted a truncated chunk log")
	}
}

func TestChunkLogRejectsEditedRecord(t *testing.T) {
	dir := t.TempDir()
	log := NewChunkLog(filepath.Join(dir, "job.ckpt"))
	chain := [32]byte{}
	for i := 0; i < 3; i++ {
		var err error
		chain, err = log.Append(ChunkRecord{Index: uint32(i), Samples: 1, Checksum: uint32(i)}, chain)
		if err != nil {
			t.Fatal(err)
		}
	}
	raw, err := os.ReadFile(log.Path())
	if err != nil {
		t.Fatal(err)
	}
	raw[24] ^= 0xFF // flip a checksum byte
	if err := os.WriteFile(log.Path(), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := log.Verify(chain, 3); err == nil {
		t.Fatal("accepted an edited chunk record")
	}
}

func TestChunkLogOutOfOrderRejected(t *testing.T) {
	dir := t.TempDir()
	log := NewChunkLog(filepath.Join(dir, "job.ckpt"))
	if _, err := log.Append(ChunkRecord{Index: 3, Samples: 1}, [32]byte{}); err != nil {
		t.Fatal(err)
	}
	if err := log.Verify([32]byte{}, 1); err == nil {
		t.Fatal("accepted an out-of-order chunk index")
	}
}
