package index

import (
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func tempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	return dir
}

func TestJournalRoundTrip(t *testing.T) {
	dir := tempDir(t)
	j, entries, err := OpenJournal(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("new journal has %d entries", len(entries))
	}
	cp := sampleCheckpoint()
	if err := j.SaveCheckpoint(cp); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	for g := 0; g < 3; g++ {
		if err := j.CommitGroup(GroupRecord{
			GroupIndex: g, Gapped: g == 1,
			SegmentHashes: [][32]byte{sha256.Sum256([]byte{byte(g)})},
		}); err != nil {
			t.Fatalf("commit %d: %v", g, err)
		}
	}
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}

	j2, replayed, err := OpenJournal(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer j2.Close()
	if len(replayed) != 4 {
		t.Fatalf("replayed %d records, want 4", len(replayed))
	}
	st, err := Load(replayed)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(st.Groups) != 3 || st.Groups[1].Gapped != true {
		t.Fatalf("groups wrong: %+v", st.Groups)
	}
}

func TestTornTailIsSafelyIgnored(t *testing.T) {
	dir := tempDir(t)
	j, _, err := OpenJournal(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := j.SaveCheckpoint(sampleCheckpoint()); err != nil {
		t.Fatal(err)
	}
	if err := j.CommitGroup(GroupRecord{GroupIndex: 0,
		SegmentHashes: [][32]byte{sha256.Sum256([]byte("a"))}}); err != nil {
		t.Fatal(err)
	}
	path := j.Path()
	j.Close()

	// Simulate a crash mid-write: append a partial record and junk bytes.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("AVPR\x01\x00\x00\x00partial")); err != nil {
		t.Fatal(err)
	}
	f.Close()

	j2, replayed, err := OpenJournal(dir)
	if err != nil {
		t.Fatalf("reopen after torn write: %v", err)
	}
	defer j2.Close()
	st, err := Load(replayed)
	if err != nil {
		t.Fatalf("load after torn write: %v", err)
	}
	if len(st.Groups) != 1 {
		t.Fatalf("want exactly one intact group, got %d", len(st.Groups))
	}
	// The journal file must have been truncated back to record boundaries.
	info, _ := os.Stat(path)
	if info.Size()%recordSize != 0 {
		t.Fatalf("journal size %d is not a record-size multiple", info.Size())
	}
}

func TestCheckpointIdentityRejectsStaleState(t *testing.T) {
	cp := sampleCheckpoint()
	st := &State{Checkpoint: &cp}

	if err := st.VerifyCompatible(cp); err != nil {
		t.Fatalf("identical checkpoint rejected: %v", err)
	}
	changedInput := cp
	changedInput.InputHash = sha256.Sum256([]byte("different input"))
	if err := st.VerifyCompatible(changedInput); err == nil {
		t.Fatal("changed input hash must reject checkpoint reuse")
	}

	changedConfig := cp
	changedConfig.ConfigHash = sha256.Sum256([]byte("different config"))
	if err := st.VerifyCompatible(changedConfig); err == nil {
		t.Fatal("changed config hash must reject checkpoint reuse")
	}

	empty := &State{}
	if err := empty.VerifyCompatible(cp); !errors.Is(err, ErrNoCheckpoint) {
		t.Fatalf("empty state err = %v, want ErrNoCheckpoint", err)
	}
}

func TestLoadRejectsNonConsecutiveGroups(t *testing.T) {
	cp := sampleCheckpoint()
	cpPayload, err := encodeCheckpoint(cp)
	if err != nil {
		t.Fatal(err)
	}
	g0, _ := encodeGroup(GroupRecord{GroupIndex: 0,
		SegmentHashes: [][32]byte{sha256.Sum256([]byte("0"))}})
	g2, _ := encodeGroup(GroupRecord{GroupIndex: 2,
		SegmentHashes: [][32]byte{sha256.Sum256([]byte("2"))}})
	entries := []Entry{
		{Type: RecCheckpoint, Payload: cpPayload},
		{Type: RecGroup, Payload: g0},
		{Type: RecGroup, Payload: g2},
	}
	if _, err := Load(entries); err == nil {
		t.Fatal("non-consecutive group indices must fail to load")
	}
}

func TestGroupConsistencyAndConfigDigest(t *testing.T) {
	if err := EnsureGroupConsistent([][32]byte{
		sha256.Sum256([]byte("a")), sha256.Sum256([]byte("b")),
	}, 2); err != nil {
		t.Fatalf("complete group rejected: %v", err)
	}
	if err := EnsureGroupConsistent([][32]byte{
		sha256.Sum256([]byte("a")),
	}, 2); err == nil {
		t.Fatal("incomplete group must be rejected")
	}
	d1 := ConfigDigest([]int{180, 450, 900}, 30, 1000)
	d2 := ConfigDigest([]int{180, 450, 900}, 30, 1000)
	d3 := ConfigDigest([]int{180, 450}, 30, 1000)
	if d1 != d2 {
		t.Fatal("config digest not deterministic")
	}
	if d1 == d3 {
		t.Fatal("different targets produced identical config digest")
	}
}

func TestHashFileDetectsTampering(t *testing.T) {
	dir := tempDir(t)
	path := filepath.Join(dir, "seg.m4s")
	if err := os.WriteFile(path, []byte("segment-bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	h1, err := HashFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("segment-bytesX"), 0o600); err != nil {
		t.Fatal(err)
	}
	h2, err := HashFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if h1 == h2 {
		t.Fatal("hash must change when the segment file changes")
	}
}

func sampleCheckpoint() Checkpoint {
	return Checkpoint{
		InputHash:  sha256.Sum256([]byte("input")),
		ConfigHash: ConfigDigest([]int{180, 450, 900}, 30, 1000),
		TargetKbps: []int{180, 450, 900},
		FPS:        30,
		SegmentMS:  1000,
	}
}
