package verify

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mediarun/internal/model"
)

func TestBytesHash(t *testing.T) {
	// Well-known SHA-256 of "abc".
	got := BytesHash([]byte("abc"))
	if got != "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" {
		t.Fatalf("hash: %s", got)
	}
	if len(got) != 64 {
		t.Fatalf("length %d", len(got))
	}
}

func TestFileHashMatchesBytes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x.bin")
	payload := make([]byte, 100000)
	for i := range payload {
		payload[i] = byte(i)
	}
	if err := os.WriteFile(path, payload, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := FileHash(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != BytesHash(payload) {
		t.Fatalf("file hash mismatch: %s", got)
	}
	if _, err := FileHash(filepath.Join(dir, "missing")); !os.IsNotExist(err) {
		t.Fatalf("missing: %v", err)
	}
}

func TestSegmentsReport(t *testing.T) {
	dir := t.TempDir()
	a := []byte("segment-a")
	b := []byte("segment-b")
	os.WriteFile(filepath.Join(dir, "seg-000.json"), a, 0o644)
	os.WriteFile(filepath.Join(dir, "seg-001.json"), b, 0o644)
	report, err := Segments(dir, []SegmentExpected{
		{Name: "000", Hash: BytesHash(a)},
		{Name: "001", Hash: BytesHash(a)}, // deliberately wrong
		{Name: "002", Hash: BytesHash(a)}, // missing
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.Matched() != 1 || report.AllOK() {
		t.Fatalf("report: %+v", report)
	}
	if len(report.Missing) != 1 || report.Missing[0] != "002" {
		t.Fatalf("missing: %v", report.Missing)
	}
	// The order is sorted by name for stable output.
	if report.Results[0].Name != "000" || report.Results[2].Name != "002" {
		t.Fatalf("order: %+v", report.Results)
	}
}

func TestSegmentsAllOK(t *testing.T) {
	dir := t.TempDir()
	data := []byte("ok")
	os.WriteFile(filepath.Join(dir, "seg-x.json"), data, 0o644)
	report, err := Segments(dir, []SegmentExpected{{Name: "x", Hash: BytesHash(data)}})
	if err != nil {
		t.Fatal(err)
	}
	if !report.AllOK() {
		t.Fatalf("report: %+v", report)
	}
	// An empty expectation set is not "all OK".
	empty, _ := Segments(dir, nil)
	if empty.AllOK() {
		t.Fatal("empty report claims OK")
	}
}

func TestSegmentsRejectsBadHash(t *testing.T) {
	dir := t.TempDir()
	if _, err := Segments(dir, []SegmentExpected{{Name: "x", Hash: strings.Repeat("g", 64)}}); !errors.Is(err, model.ErrInvalid) {
		t.Fatalf("short hash: %v", err)
	}
}
