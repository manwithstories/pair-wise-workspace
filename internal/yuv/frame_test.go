package yuv

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

func TestFrameRoundTripAndReader(t *testing.T) {
	f, err := NewFrame(32, 18)
	if err != nil {
		t.Fatal(err)
	}
	for i := range f.Y {
		f.Y[i] = byte(i)
	}
	for i := range f.U {
		f.U[i], f.V[i] = byte(50+i), byte(200-i)
	}
	var buf bytes.Buffer
	w := NewWriter(&buf)
	if err := w.Write(f); err != nil {
		t.Fatal(err)
	}
	if w.Bytes() != f.Size() {
		t.Fatalf("writer bytes %d want %d", w.Bytes(), f.Size())
	}
	r, err := NewReader(&buf, 32, 18, 1)
	if err != nil {
		t.Fatal(err)
	}
	got, err := r.Next()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.Y, f.Y) || !bytes.Equal(got.U, f.U) || !bytes.Equal(got.V, f.V) {
		t.Fatal("frame mismatch")
	}
	if _, err := r.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("got %v want EOF", err)
	}
}

func TestFrameRejectsBadPlanesAndShortInput(t *testing.T) {
	f, _ := NewFrame(16, 16)
	f.U = f.U[:1]
	if !errors.Is(f.Validate(), ErrInvalidFrame) {
		t.Fatal("expected plane validation failure")
	}
	if _, err := ReadFrame(bytes.NewReader(make([]byte, 10)), 16, 16); !errors.Is(err, ErrShortFrame) {
		t.Fatalf("got %v want short frame", err)
	}
	if _, err := NewFrame(15, 16); !errors.Is(err, ErrInvalidFrame) {
		t.Fatal("expected odd dimension rejection")
	}
}
