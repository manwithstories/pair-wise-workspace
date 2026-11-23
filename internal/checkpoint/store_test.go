package checkpoint

import (
	"os"
	"path/filepath"
	"testing"

	"mediasync/internal/model"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestAppendWritesAtomicallyAndIndexes(t *testing.T) {
	s := open(t)
	for i := 0; i < 3; i++ {
		if _, err := s.Append(1, []byte{byte(i), 0xaa}); err != nil {
			t.Fatal(err)
		}
	}
	samples, bytes, blocks := s.Stats()
	if samples != 3 || bytes != 6 || blocks != 3 {
		t.Fatalf("stats = %d/%d/%d", samples, bytes, blocks)
	}
	for i := 0; i < 3; i++ {
		if _, err := os.Stat(s.blockPath(uint32(i))); err != nil {
			t.Fatalf("block %d missing: %v", i, err)
		}
	}
	if _, err := os.Stat(s.indexPath()); err != nil {
		t.Fatal("index missing")
	}
	// No temp files left behind.
	matches, _ := filepath.Glob(filepath.Join(s.dir, ".tmp-*"))
	if len(matches) != 0 {
		t.Fatalf("temp files left: %v", matches)
	}
}

func TestRepeatedRunsProduceIdenticalHashes(t *testing.T) {
	payload := [][]byte{{1, 2, 3}, {4, 5}, {6}}
	var hashes [][32]byte
	for run := 0; run < 2; run++ {
		s := open(t)
		for _, p := range payload {
			if _, err := s.Append(7, p); err != nil {
				t.Fatal(err)
			}
		}
		hashes = append(hashes, s.Chain())
	}
	if hashes[0] != hashes[1] {
		t.Fatal("chain hash differs across identical runs")
	}
}

func TestResumeLoadsValidPrefix(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		if _, err := s.Append(1, []byte{byte(i)}); err != nil {
			t.Fatal(err)
		}
	}
	want := s.Chain()

	// Reopen and resume: nothing to recover.
	s2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	valid, recovered, err := s2.Load()
	if err != nil {
		t.Fatal(err)
	}
	if valid != 4 || recovered {
		t.Fatalf("valid = %d recovered = %v", valid, recovered)
	}
	if s2.Chain() != want {
		t.Fatal("loaded chain hash differs from written")
	}
	// Appending after resume continues the same chain.
	if _, err := s2.Append(1, []byte{0xff}); err != nil {
		t.Fatal(err)
	}
	if s2.Chain() == want {
		t.Fatal("chain did not advance")
	}
}

func TestCorruptBlockFallsBackToLastValid(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := s.Append(1, []byte{byte(i), 1, 2}); err != nil {
			t.Fatal(err)
		}
	}
	// Flip a byte in block 1.
	path := s.blockPath(1)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data[0] ^= 0xff
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	s3, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	valid, recovered, err := s3.Load()
	if err != nil {
		t.Fatal(err)
	}
	if !recovered || valid != 1 {
		t.Fatalf("valid = %d recovered = %v, want 1/true", valid, recovered)
	}
	if _, err := os.Stat(s.blockPath(1)); !os.IsNotExist(err) {
		t.Fatal("corrupt block should be removed")
	}
	if _, err := os.Stat(s.blockPath(2)); !os.IsNotExist(err) {
		t.Fatal("blocks after the cut should be removed")
	}
	// The surviving prefix still verifies.
	got, _, _ := s3.Stats()
	if got != 1 {
		t.Fatalf("committed samples = %d", got)
	}
}

func TestMissingBlockFileFallsBack(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := s.Append(1, []byte{byte(i)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Remove(s.blockPath(1)); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	valid, recovered, err := s2.Load()
	if err != nil {
		t.Fatal(err)
	}
	if !recovered || valid != 1 {
		t.Fatalf("valid = %d recovered = %v", valid, recovered)
	}
}

func TestCorruptIndexRejected(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append(1, []byte{1}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.indexPath(), []byte("not an index"), 0o644); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = s2.Load()
	if err == nil || model.CodeOf(err) != model.ErrCheckpointCorrupt {
		t.Fatalf("err = %v", err)
	}
	if model.ExitCode(err) != model.ExitUnrecoverable {
		t.Fatalf("exit = %d", model.ExitCode(err))
	}
}

func TestBlockSizeLimitRejected(t *testing.T) {
	s := open(t)
	s.BlockSize = 4
	if _, err := s.Append(1, []byte{1, 2, 3, 4, 5}); err == nil || model.CodeOf(err) != model.ErrSizeLimit {
		t.Fatalf("err = %v", err)
	}
}

func TestAppendSamplesCountsMultiple(t *testing.T) {
	s := open(t)
	if _, err := s.AppendSamples(1, []byte{1, 2}, 10); err != nil {
		t.Fatal(err)
	}
	samples, _, blocks := s.Stats()
	if samples != 10 || blocks != 1 {
		t.Fatalf("stats = %d samples, %d blocks", samples, blocks)
	}
}

func TestPathEscapeRejected(t *testing.T) {
	if _, err := Open("../escape"); err == nil || model.CodeOf(err) != model.ErrPathEscape {
		t.Fatalf("err = %v", err)
	}
	if _, err := Open(""); err == nil {
		t.Fatal("empty dir should fail")
	}
}

func TestReset(t *testing.T) {
	s := open(t)
	if _, err := s.Append(1, []byte{1}); err != nil {
		t.Fatal(err)
	}
	if err := s.Reset(); err != nil {
		t.Fatal(err)
	}
	samples, _, blocks := s.Stats()
	if samples != 0 || blocks != 0 {
		t.Fatalf("stats after reset = %d/%d", samples, blocks)
	}
	matches, _ := filepath.Glob(filepath.Join(s.dir, "block-*.bin"))
	if len(matches) != 0 {
		t.Fatalf("blocks remain: %v", matches)
	}
}

func TestEmptyLoad(t *testing.T) {
	s := open(t)
	valid, recovered, err := s.Load()
	if err != nil || valid != 0 || recovered {
		t.Fatalf("valid=%d recovered=%v err=%v", valid, recovered, err)
	}
}

func TestHashString(t *testing.T) {
	var h [32]byte
	h[0] = 0xab
	if got := HashString(h); len(got) != 64 || got[:2] != "ab" {
		t.Fatalf("hash string = %q", got)
	}
}
