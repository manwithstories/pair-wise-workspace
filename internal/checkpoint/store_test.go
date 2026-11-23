package checkpoint

import (
	"os"
	"path/filepath"
	"testing"

	"archive.local/archive/internal/model"
	"archive.local/archive/internal/repair"
)

func store(t *testing.T) *Store {
	t.Helper()
	s, err := NewStore(filepath.Join(t.TempDir(), "ckpt"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func sampleIndex(t *testing.T) *repair.Index {
	t.Helper()
	tracks := map[uint32][]repair.Sample{1: {}, 2: {}}
	for i := 0; i < 20; i++ {
		tracks[1] = append(tracks[1], repair.Sample{Index: i, Unified: int64(i) * 1000,
			Offset: uint64(i * 100), Size: 100, Sync: i%5 == 0})
		tracks[2] = append(tracks[2], repair.Sample{Index: i, Unified: int64(i) * 1000,
			Offset: uint64(i * 100), Size: 100, Sync: i%4 == 0})
	}
	idx, err := repair.BuildIndex(tracks, []uint32{1, 2})
	if err != nil {
		t.Fatal(err)
	}
	return idx
}

func TestSaveLoadRoundTrip(t *testing.T) {
	s := store(t)
	idx := sampleIndex(t)
	fp, err := idx.Tracks[0].BlockFingerprint(0, 2)
	if err != nil {
		t.Fatal(err)
	}
	st := State{InputDigest: "aaa", SourceID: "src-1", ConfirmedHigh: 42}
	st.MarkCompleted(BlockID{TrackID: 1, From: 0, To: 2}, fp)
	if err := s.Save(st); err != nil {
		t.Fatal(err)
	}
	got, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.ConfirmedHigh != 42 || len(got.Completed) != 1 {
		t.Fatalf("loaded = %+v", got)
	}
	if err := Validate(got, "aaa", "src-1", idx); err != nil {
		t.Fatalf("validate: %v", err)
	}
}

func TestNoCheckpointIsClean(t *testing.T) {
	s := store(t)
	st, err := s.Load()
	if err != nil || st != nil {
		t.Fatalf("st=%v err=%v", st, err)
	}
}

func TestHalfWrittenCheckpointIsInvalid(t *testing.T) {
	s := store(t)
	st := State{InputDigest: "aaa", SourceID: "src-1"}
	if err := s.Save(st); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(s.path())
	if err != nil {
		t.Fatal(err)
	}
	// Truncate mid-way: JSON is no longer parseable.
	if err := os.WriteFile(s.path(), body[:len(body)/2], 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load(); err == nil || model.CodeOf(err) != model.CodeCheckpointInvalid {
		t.Fatalf("torn load err = %v", err)
	}
	// A file whose JSON parses but whose checksum does not match is equally
	// invalid: tamper a single byte in the state payload.
	tampered := append([]byte(nil), body...)
	for i := range tampered {
		if tampered[i] == 'a' {
			tampered[i] = 'b'
			break
		}
	}
	if err := os.WriteFile(s.path(), tampered, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load(); err == nil || model.CodeOf(err) != model.CodeCheckpointInvalid {
		t.Fatalf("tampered load err = %v", err)
	}
}

func TestValidateRejectsChangedInput(t *testing.T) {
	s := store(t)
	idx := sampleIndex(t)
	st := State{InputDigest: "aaa", SourceID: "src-1"}
	if err := s.Save(st); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Load()
	if err := Validate(got, "bbb", "src-1", idx); model.CodeOf(err) != model.CodeCheckpointMismatch {
		t.Fatalf("input digest err = %v", err)
	}
	if err := Validate(got, "aaa", "src-2", idx); model.CodeOf(err) != model.CodeCheckpointMismatch {
		t.Fatalf("source id err = %v", err)
	}
}

func TestValidateRejectsChangedIndexBlock(t *testing.T) {
	idx := sampleIndex(t)
	s := store(t)
	fp, _ := idx.Tracks[0].BlockFingerprint(0, 2)
	st := State{InputDigest: "aaa", SourceID: "src"}
	st.MarkCompleted(BlockID{TrackID: 1, From: 0, To: 2}, fp)
	// Store a fingerprint that does not match the current index.
	bad := st
	bad.Completed = []Block{{ID: BlockID{TrackID: 1, From: 0, To: 2}, Fingerprint: "0000"}}
	if err := Validate(&bad, "aaa", "src", idx); model.CodeOf(err) != model.CodeCheckpointMismatch {
		t.Fatalf("block err = %v", err)
	}
	_ = s
}

func TestValidateRejectsBlockOutsideIndex(t *testing.T) {
	idx := sampleIndex(t)
	st := State{Completed: []Block{{ID: BlockID{TrackID: 9, From: 0, To: 3}}}}
	if err := Validate(&st, "a", "s", idx); model.CodeOf(err) != model.CodeCheckpointMismatch {
		t.Fatalf("missing track err = %v", err)
	}
	st2 := State{Completed: []Block{{ID: BlockID{TrackID: 1, From: 0, To: 999}}}}
	if err := Validate(&st2, "a", "s", idx); model.CodeOf(err) != model.CodeCheckpointMismatch {
		t.Fatalf("range err = %v", err)
	}
}

func TestMarkCompletedIsIdempotent(t *testing.T) {
	st := State{}
	st.MarkCompleted(BlockID{TrackID: 1, From: 0, To: 2}, "fp")
	st.MarkCompleted(BlockID{TrackID: 1, From: 0, To: 2}, "fp")
	if len(st.Completed) != 1 {
		t.Fatalf("blocks = %d", len(st.Completed))
	}
	if fp, ok := st.CompletedFingerprint(BlockID{TrackID: 1, From: 0, To: 2}); !ok || fp != "fp" {
		t.Fatal("fingerprint lookup")
	}
}

func TestCheckpointFileIsOwnerOnly(t *testing.T) {
	s := store(t)
	if err := s.Save(State{InputDigest: "a"}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(s.path())
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("mode = %v", info.Mode().Perm())
	}
	// No temporary files are left behind after a successful save.
	ents, err := os.ReadDir(s.Dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		if filepath.Ext(e.Name()) == ".tmp" {
			t.Fatalf("leftover temp file %s", e.Name())
		}
	}
}
