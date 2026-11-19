package fixture

import (
	"crypto/sha256"
	"io"
	"os"
	"path/filepath"
	"testing"

	"mediq/internal/model"
)

func TestGenerateDeterministic(t *testing.T) {
	dir := t.TempDir()
	res, err := Generate(Config{Dir: dir, Frames: 50, FrameBytes: 32, Corrupt: 20, Seed: 3})
	if err != nil {
		t.Fatal(err)
	}
	a, err := os.ReadFile(res.Files["normal"])
	if err != nil {
		t.Fatal(err)
	}
	dir2 := t.TempDir()
	res2, err := Generate(Config{Dir: dir2, Frames: 50, FrameBytes: 32, Corrupt: 20, Seed: 3})
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(res2.Files["normal"])
	if err != nil {
		t.Fatal(err)
	}
	if len(a) != len(b) {
		t.Fatalf("lengths %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("differs at byte %d", i)
		}
	}
}

func TestDamagedFixtureHasOneBadFrame(t *testing.T) {
	dir := t.TempDir()
	res, err := Generate(Config{Dir: dir, Frames: 40, FrameBytes: 64, Corrupt: 17, Seed: 1})
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(res.Files["damaged"])
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	n := 0
	for {
		hdr := make([]byte, model.RecordHeaderSize)
		if _, err := io.ReadFull(f, hdr); err != nil {
			break
		}
		rh, err := model.ParseRecordHeader(hdr)
		if err != nil {
			t.Fatal(err)
		}
		body := make([]byte, int(rh.MetaLen)+int(rh.FrameLen))
		if _, err := io.ReadFull(f, body); err != nil {
			t.Fatal(err)
		}
		if rh.Seq == 17 {
			n++
			frame := body[rh.MetaLen:]
			// The recorded hash must not describe the stored payload.
			if sha256.Sum256(frame) == rh.Hash {
				t.Fatal("corrupted payload still hashes to the recorded value")
			}
		}
	}
	if n != 1 {
		t.Fatalf("corrupt frame %d appears %d times", res.CorruptSeq, n)
	}
}

func TestTruncatedFixtureEndsMidRecord(t *testing.T) {
	dir := t.TempDir()
	res, err := Generate(Config{Dir: dir, Frames: 30, FrameBytes: 64, TruncateTail: 1, Seed: 1})
	if err != nil {
		t.Fatal(err)
	}
	if res.TruncatedBytes <= 0 {
		t.Fatalf("cut %d bytes", res.TruncatedBytes)
	}
	f, err := os.Open(res.Files["truncated"])
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	fi, _ := f.Stat()
	if fi.Size() == 0 {
		t.Fatal("empty fixture")
	}
	// The last record must be incomplete: reading it fails with EOF.
	for {
		hdr := make([]byte, model.RecordHeaderSize)
		if _, err := io.ReadFull(f, hdr); err != nil {
			break
		}
		rh, err := model.ParseRecordHeader(hdr)
		if err != nil {
			t.Fatal(err)
		}
		var body [1 << 16]byte
		if _, err := io.ReadFull(f, body[:int(rh.MetaLen)+int(rh.FrameLen)]); err != nil {
			if err != io.ErrUnexpectedEOF {
				t.Fatalf("last record ended with %v, want short body", err)
			}
			return
		}
	}
	t.Fatal("truncated fixture ended on a complete record")
}

func TestWrapFixtureRollsOver32BitClock(t *testing.T) {
	dir := t.TempDir()
	if _, err := Generate(Config{Dir: dir, Frames: 20, FrameBytes: 16, WrapAt: 10, Seed: 1}); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(filepath.Join(dir, "wrap.seq"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var stamps []uint64
	for {
		hdr := make([]byte, model.RecordHeaderSize)
		if _, err := io.ReadFull(f, hdr); err != nil {
			break
		}
		rh, err := model.ParseRecordHeader(hdr)
		if err != nil {
			t.Fatal(err)
		}
		stamps = append(stamps, rh.Timestamp)
		if _, err := f.Seek(int64(rh.MetaLen)+int64(rh.FrameLen), io.SeekCurrent); err != nil {
			t.Fatal(err)
		}
	}
	if len(stamps) != 20 {
		t.Fatalf("%d frames", len(stamps))
	}
	wrapped := false
	for i := 1; i < len(stamps); i++ {
		if stamps[i] < stamps[i-1] {
			wrapped = true
			if i != 10 {
				t.Fatalf("wrap at frame %d, want 11", i+1)
			}
		}
	}
	if !wrapped {
		t.Fatal("no wrap in the wrap fixture")
	}
	if stamps[10] != 0 {
		t.Fatalf("post-wrap timestamp %d", stamps[10])
	}
}

func TestDuplicateFixtureRepeatsOneRecord(t *testing.T) {
	dir := t.TempDir()
	if _, err := Generate(Config{Dir: dir, Frames: 20, FrameBytes: 16, Duplicate: 7, Seed: 1}); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(filepath.Join(dir, "duplicate.seq"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	seen := map[uint64]int{}
	for {
		hdr := make([]byte, model.RecordHeaderSize)
		if _, err := io.ReadFull(f, hdr); err != nil {
			break
		}
		rh, err := model.ParseRecordHeader(hdr)
		if err != nil {
			t.Fatal(err)
		}
		seen[rh.Seq]++
		if _, err := f.Seek(int64(rh.MetaLen)+int64(rh.FrameLen), io.SeekCurrent); err != nil {
			t.Fatal(err)
		}
	}
	if seen[7] != 2 {
		t.Fatalf("frame 7 appears %d times", seen[7])
	}
	if len(seen) != 20 {
		t.Fatalf("%d distinct frames", len(seen))
	}
}
