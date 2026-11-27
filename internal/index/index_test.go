package index

import (
	"os"
	"testing"

	"avrepair/internal/clock"
)

func TestBuildAndSeek(t *testing.T) {
	dir := t.TempDir()
	b, err := NewBuilder(dir, "media.idx")
	if err != nil {
		t.Fatal(err)
	}
	entries := []Entry{
		{Track: "v", Seq: 0, TS: 0, Offset: 0, Length: 100},
		{Track: "a", Seq: 0, TS: 1_000_000, Offset: 100, Length: 50},
		{Track: "v", Seq: 1, TS: 33_333_333, Offset: 150, Length: 120},
		{Track: "v", Seq: 2, TS: 66_666_666, Offset: 270, Length: 110},
	}
	for _, e := range entries {
		if err := b.Append(e); err != nil {
			t.Fatalf("append %+v: %v", e, err)
		}
	}
	idx, err := b.Commit()
	if err != nil {
		t.Fatal(err)
	}
	if idx.Count() != len(entries) {
		t.Fatalf("count=%d want %d", idx.Count(), len(entries))
	}

	// Exact and predecessor seeks on the video track.
	got, miss, err := idx.Seek("v", 33_333_333)
	if err != nil || miss != 0 || got.Seq != 1 {
		t.Fatalf("exact seek failed: %+v miss=%d err=%v", got, miss, err)
	}
	got, miss, _ = idx.Seek("v", 40_000_000)
	if miss != 0 || got.Seq != 1 {
		t.Fatalf("predecessor seek wrong: %+v miss=%d", got, miss)
	}
	// Track filter must exclude audio entries.
	got, _, _ = idx.Seek("a", 5_000_000)
	if got.Track != "a" || got.Seq != 0 {
		t.Fatalf("audio seek wrong: %+v", got)
	}
	// A target before the first entry is a genuine miss.
	if _, miss, err := idx.Seek("v", -1); err != nil || miss != 1 {
		t.Fatalf("pre-first seek miss=%d err=%v", miss, err)
	}
}

func TestRejectsNonMonotonic(t *testing.T) {
	dir := t.TempDir()
	b, _ := NewBuilder(dir, "bad.idx")
	_ = b.Append(Entry{Track: "v", Seq: 0, TS: 100})
	err := b.Append(Entry{Track: "v", Seq: 1, TS: 100}) // equal -> reject
	if err == nil || !clock.HasCode(err, clock.CodeNonMonotonic) {
		t.Fatalf("equal timestamps must fail: %v", err)
	}
	b.Abort()
}

func TestTruncatedTempRecoverable(t *testing.T) {
	dir := t.TempDir()
	finalName := "trunc.idx"

	// First run: build and commit a fully verified prefix.
	b1, _ := NewBuilder(dir, finalName)
	for i := 0; i < 10; i++ {
		_ = b1.Append(Entry{Track: "v", Seq: i, TS: int64(i+1) * clock.Millisecond,
			Offset: int64(i * 10), Length: 10})
	}
	good, err := b1.Commit()
	if err != nil {
		t.Fatal(err)
	}

	// Simulate an interrupted second run leaving a temp file behind; the
	// committed index must remain intact and openable.
	b2, _ := NewBuilder(dir, finalName)
	_ = b2.Append(Entry{Track: "v", Seq: 99, TS: 99 * clock.Second})
	b2.Abort() // crash before commit

	reopened, err := Open(dir, finalName)
	if err != nil {
		t.Fatalf("committed index must survive interruption: %v", err)
	}
	if reopened.Count() != good.Count() {
		t.Fatalf("count changed after aborted run: %d vs %d",
			reopened.Count(), good.Count())
	}
	got, miss, err := reopened.Seek("v", 5*clock.Millisecond)
	if err != nil || miss != 0 || got.Seq != 4 {
		t.Fatalf("seek after recovery failed: %+v miss=%d err=%v", got, miss, err)
	}
}

func TestCorruptionDetected(t *testing.T) {
	dir := t.TempDir()
	b, _ := NewBuilder(dir, "corrupt.idx")
	_ = b.Append(Entry{Track: "v", Seq: 0, TS: 1_000, Offset: 0, Length: 10})
	idx, err := b.Commit()
	if err != nil {
		t.Fatal(err)
	}
	// Flip a byte inside the timestamp field (after header + track field).
	data, _ := os.ReadFile(idx.Path())
	tsByte := len(Magic) + 2 + 2 + trackField + 4 // header + len + track + seq
	data[tsByte] ^= 0xFF
	if err := os.WriteFile(idx.Path(), data, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir, "corrupt.idx"); err == nil ||
		!clock.HasCode(err, clock.CodeChecksum) {
		t.Fatalf("corruption must raise CodeChecksum, got %v", err)
	}
}
