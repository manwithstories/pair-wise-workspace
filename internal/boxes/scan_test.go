package boxes

import (
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"testing"

	"bcast/internal/fixture"
	"bcast/internal/media"
)

func genConfig(t *testing.T, mutate func(*fixture.Config)) string {
	t.Helper()
	dir := t.TempDir()
	cfg := fixture.CaseConfig("clean", dir)
	cfg.FragmentCount = 3
	cfg.SamplesPerFrag = 8
	cfg.SampleSize = 64
	cfg.UniformDuration = 3000
	if mutate != nil {
		mutate(&cfg)
	}
	if err := fixture.GenerateConfig(cfg); err != nil {
		t.Fatalf("generate: %v", err)
	}
	return dir
}

func TestScanNormalFixture(t *testing.T) {
	dir := genConfig(t, nil)
	rep, err := ScanBatch(dir, ScanOptions{Concurrency: 2})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(rep.Fragments) != 3 || len(rep.Unique) != 3 {
		t.Fatalf("got fragments=%d unique=%d", len(rep.Fragments), len(rep.Unique))
	}
	for _, f := range rep.Unique {
		if len(f.Samples) != 8 {
			t.Fatalf("seq %d samples=%d", f.Sequence, len(f.Samples))
		}
		if f.ContentSHA256 == "" {
			t.Fatalf("seq %d missing digest", f.Sequence)
		}
		if f.MoofOffset != 0 {
			t.Fatalf("moof offset=%d, want 0", f.MoofOffset)
		}
	}
}

func TestScanTruncatedBoxReportsFirstByteAndFragment(t *testing.T) {
	dir := t.TempDir()
	if _, err := fixture.Generate("truncated", dir); err != nil {
		t.Fatal(err)
	}
	rep, err := ScanBatch(dir, ScanOptions{})
	if err == nil {
		t.Fatal("expected corruption error")
	}
	me, ok := media.AsError(err)
	if !ok || me.ExitCode != media.ExitCorrupt {
		t.Fatalf("error=%v, want media corrupt", err)
	}
	if me.Fragment != 2 {
		t.Fatalf("fragment=%d, want 2", me.Fragment)
	}
	if me.Offset <= 0 {
		t.Fatalf("offset=%d, want positive first error byte", me.Offset)
	}
	if rep == nil || rep.Fatal == nil || rep.Fatal.Offset != me.Offset {
		t.Fatalf("report fatal=%v, want offset %d", rep.Fatal, me.Offset)
	}
	if len(rep.Isolated) != 1 {
		t.Fatalf("isolated=%d, want 1", len(rep.Isolated))
	}
}

func TestDuplicateDigestGapAndOutOfOrder(t *testing.T) {
	dir := t.TempDir()
	if _, err := fixture.Generate("duplicate", dir); err != nil {
		t.Fatal(err)
	}
	rep, err := ScanBatch(dir, ScanOptions{})
	if err != nil {
		t.Fatalf("duplicate content should be deduplicated not fatal: %v", err)
	}
	if got := rep.Sequences.DuplicateCount(); got != 1 {
		t.Fatalf("duplicates=%d, want 1", got)
	}
	if len(rep.Sequences.OutOfOrder) == 0 {
		t.Fatal("expected out-of-order mfhd")
	}
	if rep.Sequences.MissingCount() != 1 {
		t.Fatalf("missing=%d, want 1", rep.Sequences.MissingCount())
	}
	if len(rep.Unique) != 5 {
		t.Fatalf("unique=%d, want 5 after gap and digest dedup", len(rep.Unique))
	}

	// Scanning twice must produce the same unique index inputs.
	rep2, err := ScanBatch(dir, ScanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep2.Unique) != len(rep.Unique) {
		t.Fatalf("second scan unique=%d want %d", len(rep2.Unique), len(rep.Unique))
	}
	for i := range rep.Unique {
		if rep.Unique[i].ContentSHA256 != rep2.Unique[i].ContentSHA256 {
			t.Fatalf("unique fragment %d changed between scans", i)
		}
	}
}

func TestOversizedAndShortBoxAreLocated(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "seg-00001.m4s")
	b := make([]byte, 16)
	binary.BigEndian.PutUint32(b[0:4], 1) // 64-bit largesize
	copy(b[4:8], "mdat")
	binary.BigEndian.PutUint64(b[8:16], math.MaxUint64)
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
	frag, err := parseSegment(path, 1, map[uint32]uint32{1: 90000})
	if err == nil {
		t.Fatal("expected oversized box error")
	}
	me, ok := media.AsError(err)
	if !ok || me.ExitCode != media.ExitCorrupt {
		t.Fatalf("err=%v want corrupt", err)
	}
	if me.Offset != int64(len(b)) {
		t.Fatalf("offset=%d want EOF/first unavailable %d", me.Offset, len(b))
	}
	if frag == nil {
		t.Fatal("expected partial fragment for isolation")
	}

	short := filepath.Join(dir, "seg-00002.m4s")
	if err := os.WriteFile(short, []byte{0, 0, 0}, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = parseSegment(short, 2, map[uint32]uint32{1: 90000})
	if me, ok := media.AsError(err); !ok || me.Offset != 3 {
		t.Fatalf("short header err=%v offset location mismatch", err)
	}
}
