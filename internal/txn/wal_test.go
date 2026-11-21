package txn

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAppendAndReopen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wal.log")
	w, scan, err := OpenWAL(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(scan.Records) != 0 || scan.ValidBytes == 0 {
		t.Fatalf("scan=%+v", scan)
	}
	if _, err := w.Append(KindBegin, "probe", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Append(KindData, "probe", []byte("hello")); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Append(KindCommit, "probe", []byte("cp")); err != nil {
		t.Fatal(err)
	}
	w.Close()

	w2, scan2, err := OpenWAL(path)
	if err != nil {
		t.Fatal(err)
	}
	defer w2.Close()
	if len(scan2.Records) != 3 {
		t.Fatalf("records=%d", len(scan2.Records))
	}
	if scan2.Records[1].Payload[0] != 'h' || scan2.Records[1].Seq != 2 {
		t.Fatalf("record=%+v", scan2.Records[1])
	}
	if scan2.CorruptBytes != 0 {
		t.Fatalf("unexpected corruption: %+v", scan2)
	}
}

func TestTornTailIsTruncated(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wal.log")
	w, _, err := OpenWAL(path)
	if err != nil {
		t.Fatal(err)
	}
	w.Append(KindBegin, "probe", nil)
	w.Append(KindData, "probe", []byte("payload"))
	valid := w.size
	w.Append(KindCommit, "probe", []byte("checkpoint"))
	w.Close()

	// Simulate a crash mid-write: chop the last frame in half.
	full, _ := os.ReadFile(path)
	if err := os.WriteFile(path, full[:len(full)-3], 0o644); err != nil {
		t.Fatal(err)
	}

	w2, scan, err := OpenWAL(path)
	if err != nil {
		t.Fatal(err)
	}
	defer w2.Close()
	if len(scan.Records) != 2 {
		t.Fatalf("records=%d want 2", len(scan.Records))
	}
	if scan.CorruptBytes == 0 || scan.CorruptReason == "" {
		t.Fatalf("torn tail not reported: %+v", scan)
	}
	if scan.ValidBytes != valid {
		t.Fatalf("valid bytes=%d want %d", scan.ValidBytes, valid)
	}
}

func TestBitFlipIsDetected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wal.log")
	w, _, err := OpenWAL(path)
	if err != nil {
		t.Fatal(err)
	}
	w.Append(KindBegin, "probe", nil)
	w.Append(KindData, "probe", []byte("payload"))
	w.Close()

	data, _ := os.ReadFile(path)
	data[len(data)-6] ^= 0xff // flip a payload byte
	os.WriteFile(path, data, 0o644)

	_, scan, err := OpenWAL(path)
	if err != nil {
		t.Fatal(err)
	}
	if scan.CorruptRecords == 0 || scan.CorruptBytes == 0 {
		t.Fatalf("bit flip not detected: %+v", scan)
	}
	if len(scan.Records) != 1 {
		t.Fatalf("records=%d want 1", len(scan.Records))
	}
}

func TestBadHeader(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wal.log")
	os.WriteFile(path, []byte("NOTAWAL!!"), 0o644)
	if _, _, err := OpenWAL(path); err == nil {
		t.Fatal("expected bad header error")
	}
}

func TestSequenceGapIsCorruption(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wal.log")
	w, _, _ := OpenWAL(path)
	w.Append(KindBegin, "probe", nil)
	w.Append(KindData, "probe", nil)
	w.Close()
	// Drop the second record's sequence by rewriting it: simplest is to
	// append a hand-built frame with the wrong sequence.
	if err := os.WriteFile(path, appendFileWithBadSeq(t, path), 0o644); err != nil {
		t.Fatal(err)
	}
	_, scan, err := OpenWAL(path)
	if err != nil {
		t.Fatal(err)
	}
	if scan.CorruptRecords == 0 {
		t.Fatalf("sequence gap not detected: %+v", scan)
	}
}

func appendFileWithBadSeq(t *testing.T, path string) []byte {
	t.Helper()
	data, _ := os.ReadFile(path)
	body := []byte{byte(KindBegin), 0, 5, 'p', 'r', 'o', 'b', 'e'}
	body = append(body, 0, 0, 0, 0, 0, 0, 0, 99)
	frame := make([]byte, 4)
	frame[0], frame[1], frame[2], frame[3] = 0, 0, 0, byte(len(body))
	frame = append(frame, body...)
	frame = append(frame, 0, 0, 0, 0)
	return append(data, frame...)
}
