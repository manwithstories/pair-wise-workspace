package txn

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWALReplaysOnlyChecksummedRecordsAndTornTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wal.log")
	w, err := OpenWAL(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Append("prepared", "tx1", "a", "sha", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Append("committed", "tx1", "a", "sha", 1); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	// A torn final append (JSON without newline/checksum) must be ignored.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"seq":3,"stage":"prepared","checksum":"bad`); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	res, err := ReplayWAL(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Records) != 2 || !res.TornTail {
		t.Fatalf("records=%d torn=%v", len(res.Records), res.TornTail)
	}
	for _, r := range res.Records {
		if !r.VerifyChecksum() {
			t.Fatalf("record %d bad checksum", r.Seq)
		}
	}
}

func TestWALRejectsCorruptCompleteRecord(t *testing.T) {
	r := Record{Seq: 1, Stage: "committed", Name: "x", SHA256: "abc"}
	r.Checksum = RecordChecksum(r)
	if !r.VerifyChecksum() {
		t.Fatal("fresh checksum invalid")
	}
	r.SHA256 = "changed"
	if r.VerifyChecksum() {
		t.Fatal("tampered record passed checksum")
	}
}
