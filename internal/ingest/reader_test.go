package ingest

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

func TestReaderWithinBudget(t *testing.T) {
	src := bytes.NewReader(make([]byte, 100))
	r, err := NewReader(src, 100)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("within-budget read failed: %v", err)
	}
	if len(data) != 100 {
		t.Fatalf("want 100 bytes, got %d", len(data))
	}
}

func TestReaderRejectsOverBudget(t *testing.T) {
	src := bytes.NewReader(make([]byte, 250))
	r, _ := NewReader(src, 100)
	_, err := io.ReadAll(r)
	if !errors.Is(err, ErrLimitExceeded) {
		t.Fatalf("want ErrLimitExceeded, got %v", err)
	}
	if r.BytesRead() < 100 {
		t.Fatalf("budget accounting wrong: %d", r.BytesRead())
	}
}

func TestReaderExactBoundaryThenMore(t *testing.T) {
	src := bytes.NewReader(make([]byte, 101))
	r, _ := NewReader(src, 100)
	_, err := io.ReadAll(r)
	if !errors.Is(err, ErrLimitExceeded) {
		t.Fatalf("oversized stream must be rejected at boundary, got %v", err)
	}
}

func TestReadAtMostBounded(t *testing.T) {
	src := bytes.NewReader([]byte("hello world"))
	r, _ := NewReader(src, 64)
	buf, err := r.ReadAtMost(5)
	if err != nil && !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
	if len(buf) != 5 {
		t.Fatalf("want 5 bytes, got %d", len(buf))
	}
}

func TestInvalidLimit(t *testing.T) {
	if _, err := NewReader(bytes.NewReader(nil), 0); err == nil {
		t.Fatal("zero limit must be rejected")
	}
}
