package index

import (
	"errors"
	"testing"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestAtomicWriteAndRead(t *testing.T) {
	s := newTestStore(t)
	payload := map[string]int{"a": 1, "b": 2}
	if err := s.AtomicWrite("x.json", payload); err != nil {
		t.Fatal(err)
	}
	var got map[string]int
	if err := s.ReadJSON("x.json", &got); err != nil {
		t.Fatal(err)
	}
	if got["b"] != 2 {
		t.Fatalf("round trip mismatch: %v", got)
	}
	// No temp files may linger after a successful atomic replace.
	ents, _ := readDirNames(s.Dir())
	for _, e := range ents {
		if len(e) > 4 && e[:4] == ".tmp" {
			t.Fatalf("temp file leaked: %s", e)
		}
	}
}

func TestReadMissingReturnsNotFound(t *testing.T) {
	s := newTestStore(t)
	var v interface{}
	if err := s.ReadJSON("nope.json", &v); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestCheckpointChunkBoundary(t *testing.T) {
	c := NewChunker("digest-1", DefaultCheckpointEvery)
	due := 0
	for i := 0; i < DefaultCheckpointEvery*2+3; i++ {
		if c.Observe("in.mp4", i) {
			due++
		}
	}
	if due != 2 {
		t.Fatalf("checkpoint due count = %d, want 2", due)
	}
	if c.Chunks() != 2 || c.Samples() != DefaultCheckpointEvery*2+3 {
		t.Fatalf("chunk/sample counts wrong: %d %d", c.Chunks(), c.Samples())
	}
}

func TestResumeRejectsForeignDigest(t *testing.T) {
	s := newTestStore(t)
	c := NewChunker("digest-A", DefaultCheckpointEvery)
	for i := 0; i < 300; i++ {
		c.Observe("a.mp4", i)
	}
	if err := c.Save(s); err != nil {
		t.Fatal(err)
	}
	// A different input must be refused, never silently resumed.
	if _, err := LoadCheckpoint(s, "digest-B"); !errors.Is(err, ErrDigest) {
		t.Fatalf("want ErrDigest, got %v", err)
	}
}

func TestResumeIsIdempotentAndChecksummed(t *testing.T) {
	s := newTestStore(t)
	c := NewChunker("digest-A", DefaultCheckpointEvery)
	for i := 0; i < 300; i++ {
		c.Observe("a.mp4", i)
	}
	c.CommitSegment("seg-0")
	c.CommitSegment("seg-0") // duplicate must not double count
	c.CommitSegment("seg-1")
	if err := c.Save(s); err != nil {
		t.Fatal(err)
	}
	cp, err := LoadCheckpoint(s, "digest-A")
	if err != nil {
		t.Fatal(err)
	}
	resumed := Resume(*cp)
	if resumed.SegmentCount() != 2 {
		t.Fatalf("resumed segment count = %d, want 2 (no double count)", resumed.SegmentCount())
	}
	if resumed.Samples() != 300 {
		t.Fatalf("resumed samples = %d, want 300", resumed.Samples())
	}
	// Tampering with the persisted file must fail checksum validation.
	tamperStoreFile(t, s, checkpointName)
	if _, err := LoadCheckpoint(s, "digest-A"); !errors.Is(err, ErrChecksum) {
		t.Fatalf("want ErrChecksum after tamper, got %v", err)
	}
}

func TestDigestJSONStable(t *testing.T) {
	v := map[string]int{"x": 1}
	d1, b1, err := DigestJSON(v)
	if err != nil {
		t.Fatal(err)
	}
	d2, b2, err := DigestJSON(v)
	if err != nil {
		t.Fatal(err)
	}
	if d1 != d2 || string(b1) != string(b2) {
		t.Fatal("digest must be byte stable across calls")
	}
}
