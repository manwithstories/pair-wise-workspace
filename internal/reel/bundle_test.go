package reel

import (
	"bytes"
	"errors"
	"testing"
)

func TestGenerateDeterministicAndRoundTrip(t *testing.T) {
	a, err := Generate(7, 2)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Generate(7, 2)
	if err != nil {
		t.Fatal(err)
	}
	dataA, err := a.Encode()
	if err != nil {
		t.Fatal(err)
	}
	dataB, err := b.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(dataA, dataB) {
		t.Fatal("same seed produced different bytes")
	}
	decoded, err := Decode(dataA)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Width != DefaultWidth || decoded.Height != DefaultHeight || decoded.FramesPerSeg != DefaultFramesPerSeg {
		t.Fatalf("unexpected decoded header: %+v", decoded)
	}
	if len(decoded.Segments) != 2 || len(decoded.Segments[1]) != DefaultFramesPerSeg {
		t.Fatal("decoded segment shape mismatch")
	}
}

func TestDecodeRejectsCorruption(t *testing.T) {
	b, err := Generate(11, 1)
	if err != nil {
		t.Fatal(err)
	}
	data, err := b.Encode()
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)/2] ^= 0xff
	if _, err := Decode(data); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("got %v, want ErrCorrupt", err)
	}
}
