package index

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func commitN(t *testing.T, s *Store, outPath string, n int) {
	t.Helper()
	for k := 0; k < n; k++ {
		cursor := int64((k + 1) * 10)
		tail, m, err := HashPrefix(outPath, cursor)
		if err != nil || m != cursor {
			t.Fatalf("hash prefix: %v %d", err, m)
		}
		cp := Checkpoint{
			Chunk:          k,
			InputCursor:    int64((k + 1) * 64),
			OutputCursor:   cursor,
			ChunkHash:      fmt.Sprintf("chunk-%d", k),
			OutputTailHash: tail,
		}
		if err := s.Commit(cp); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCommitAndRecover(t *testing.T) {
	dir := t.TempDir()
	outPath := filepath.Join(dir, "out.bin")
	if err := os.WriteFile(outPath, make([]byte, 100), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Open(filepath.Join(dir, "idx"))
	if err != nil {
		t.Fatal(err)
	}
	commitN(t, s, outPath, 3)
	verify := func(cp Checkpoint) bool {
		got, m, err := HashPrefix(outPath, cp.OutputCursor)
		return err == nil && m == cp.OutputCursor && got == cp.OutputTailHash
	}
	cp, ok, err := s.Recover(verify)
	if err != nil || !ok || cp.Chunk != 2 {
		t.Fatalf("recover = %+v %v %v", cp, ok, err)
	}
}

func TestRecoverDropsCorruptTail(t *testing.T) {
	dir := t.TempDir()
	outPath := filepath.Join(dir, "out.bin")
	if err := os.WriteFile(outPath, make([]byte, 100), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Open(filepath.Join(dir, "idx"))
	if err != nil {
		t.Fatal(err)
	}
	commitN(t, s, outPath, 3)
	// Corrupt the newest checkpoint file.
	p, _ := s.Path(2)
	b, _ := os.ReadFile(p)
	b[10] ^= 0xff
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	verify := func(cp Checkpoint) bool {
		got, m, err := HashPrefix(outPath, cp.OutputCursor)
		return err == nil && m == cp.OutputCursor && got == cp.OutputTailHash
	}
	cp, ok, err := s.Recover(verify)
	if err != nil || !ok || cp.Chunk != 1 {
		t.Fatalf("recover = %+v %v %v", cp, ok, err)
	}
	nums, _ := s.List()
	if fmt.Sprint(nums) != "[0 1]" {
		t.Fatalf("remaining = %v", nums)
	}
}

func TestRecoverTruncatedOutput(t *testing.T) {
	dir := t.TempDir()
	outPath := filepath.Join(dir, "out.bin")
	if err := os.WriteFile(outPath, make([]byte, 100), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Open(filepath.Join(dir, "idx"))
	if err != nil {
		t.Fatal(err)
	}
	commitN(t, s, outPath, 3)
	// Damage the output tail: the last checkpoint can no longer verify.
	if err := os.Truncate(outPath, 25); err != nil {
		t.Fatal(err)
	}
	verify := func(cp Checkpoint) bool {
		got, m, err := HashPrefix(outPath, cp.OutputCursor)
		return err == nil && m == cp.OutputCursor && got == cp.OutputTailHash
	}
	cp, ok, err := s.Recover(verify)
	if err != nil || !ok || cp.Chunk != 1 {
		t.Fatalf("recover = %+v %v %v", cp, ok, err)
	}
}

func TestSafeJoinRejectsTraversal(t *testing.T) {
	dir := t.TempDir()
	for _, bad := range []string{"../x", "a/b", "..", "/etc/passwd", ""} {
		if _, err := SafeJoin(dir, bad); err == nil {
			t.Fatalf("SafeJoin(%q) unexpectedly succeeded", bad)
		}
	}
	if _, err := SafeJoin(dir, "chk_000001.json"); err != nil {
		t.Fatal(err)
	}
}
