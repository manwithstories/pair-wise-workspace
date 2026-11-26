package report_test

import (
	"path/filepath"
	"testing"

	"mediascan/internal/report"
)

func TestCanonicalHashIgnoresTiming(t *testing.T) {
	r := &report.Report{
		SchemaVersion: report.SchemaVersion,
		Metrics:       report.Metrics{Files: 6, Tracks: 12},
	}
	first, hash1, err := report.CanonicalHash(r)
	if err != nil {
		t.Fatal(err)
	}

	r.Timing.ResumeMS = 999
	r.Timing.ScanMS = 123456
	second, hash2, err := report.CanonicalHash(r)
	if err != nil {
		t.Fatal(err)
	}
	if hash1 != hash2 {
		t.Fatalf("timing changed hash: %s != %s", hash1, hash2)
	}
	if string(first) != string(second) {
		t.Fatal("canonical bytes changed despite json:\"-\" timing")
	}

	path := filepath.Join(t.TempDir(), "report.json")
	writtenHash, err := report.WriteCanonical(path, r)
	if err != nil {
		t.Fatal(err)
	}
	fileHash, err := report.ReadHash(path)
	if err != nil {
		t.Fatal(err)
	}
	if writtenHash != hash1 || fileHash != hash1 {
		t.Fatalf("hashes disagree canonical=%s written=%s file=%s", hash1, writtenHash, fileHash)
	}
}
