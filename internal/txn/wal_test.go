package txn

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mediarun/internal/model"
)

func TestWALAppendReplay(t *testing.T) {
	dir := t.TempDir()
	w, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Append(Record{Op: OpBegin, Stage: "ingest"}); err != nil {
		t.Fatal(err)
	}
	if err := w.Append(Record{Op: OpStage, Segment: "seg-1", Hash: strings.Repeat("a", 64)}); err != nil {
		t.Fatal(err)
	}
	if w.Seq() != 2 {
		t.Fatalf("seq %d", w.Seq())
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	records, truncated, err := Replay(dir)
	if err != nil {
		t.Fatal(err)
	}
	if truncated {
		t.Fatal("unexpected truncation")
	}
	if len(records) != 2 || records[1].Segment != "seg-1" || records[0].Op != OpBegin {
		t.Fatalf("records: %+v", records)
	}
}

func TestWALRejectsBadRecords(t *testing.T) {
	dir := t.TempDir()
	w, _ := Open(dir)
	defer w.Close()
	if err := w.Append(Record{}); !errors.Is(err, model.ErrInvalid) {
		t.Fatalf("empty op: %v", err)
	}
	if err := w.Append(Record{Op: OpBegin, Checksum: "abc"}); !errors.Is(err, model.ErrInvalid) {
		t.Fatalf("caller checksum: %v", err)
	}
	long := strings.Repeat("x", MaxRecordBytes)
	if err := w.Append(Record{Op: OpCheck, Detail: long}); !errors.Is(err, model.ErrLimit) {
		t.Fatalf("oversize: %v", err)
	}
}

func TestReplayToleratesPartialTail(t *testing.T) {
	dir := t.TempDir()
	w, _ := Open(dir)
	w.Append(Record{Op: OpBegin, Stage: "a"})
	w.Close()
	// Simulate a crash mid-write: append an unterminated fragment.
	f, err := os.OpenFile(filepath.Join(dir, "wal.jsonl"), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(`{"seq":2,"op":"commit","segm`)
	f.Close()
	records, truncated, err := Replay(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !truncated {
		t.Fatal("expected truncation flag")
	}
	if len(records) != 1 {
		t.Fatalf("records %d", len(records))
	}
}

func TestReplayDetectsTamper(t *testing.T) {
	dir := t.TempDir()
	w, _ := Open(dir)
	w.Append(Record{Op: OpBegin, Stage: "a"})
	w.Close()
	path := filepath.Join(dir, "wal.jsonl")
	data, _ := os.ReadFile(path)
	tampered := strings.Replace(string(data), `"stage":"a"`, `"stage":"b"`, 1)
	os.WriteFile(path, []byte(tampered), 0o644)
	if _, _, err := Replay(dir); !errors.Is(err, model.ErrCorruptIndex) {
		t.Fatalf("tamper: %v", err)
	}
	// Garbage line.
	os.WriteFile(path, []byte("not json\n"), 0o644)
	if _, _, err := Replay(dir); !errors.Is(err, model.ErrCorruptIndex) {
		t.Fatalf("garbage: %v", err)
	}
	// Out-of-order sequence.
	os.WriteFile(path, []byte(`{"seq":5,"op":"begin","checksum":"00"}`+"\n"), 0o644)
	if _, _, err := Replay(dir); !errors.Is(err, model.ErrCorruptIndex) {
		t.Fatalf("sequence: %v", err)
	}
}

func TestReplayMissingDir(t *testing.T) {
	records, truncated, err := Replay(filepath.Join(t.TempDir(), "nope"))
	if err != nil || truncated || len(records) != 0 {
		t.Fatalf("missing: %v %v %d", err, truncated, len(records))
	}
}

func TestWALReset(t *testing.T) {
	dir := t.TempDir()
	w, _ := Open(dir)
	w.Append(Record{Op: OpBegin})
	w.Close()
	if err := Reset(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "wal.jsonl")); !os.IsNotExist(err) {
		t.Fatal("wal not removed")
	}
	if err := Reset(dir); err != nil {
		t.Fatalf("reset twice: %v", err)
	}
}

func TestOpenRejectsEmptyDir(t *testing.T) {
	if _, err := Open(""); !errors.Is(err, model.ErrInvalid) {
		t.Fatalf("empty dir: %v", err)
	}
}
