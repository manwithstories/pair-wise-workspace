package mp4box

import (
	"bytes"
	"testing"

	"mediad/diag"
)

func TestEncodeDecodeRoundTrip(t *testing.T) {
	payload := []byte("MEDIAD-SEG rep=v-600 segment=7 frame=630\n")
	raw, err := Encode(payload)
	if err != nil {
		t.Fatal(err)
	}
	// A real fMP4 fragment starts with an ftyp box ('ftyp' at offset 4).
	if len(raw) < 12 || string(raw[4:8]) != "ftyp" {
		t.Fatalf("output missing ftyp box header: % x", raw[:12])
	}
	if !bytes.Contains(raw, []byte("mdat")) {
		t.Fatal("output missing mdat box")
	}
	got, brand, err := Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	if brand != "iso5" {
		t.Fatalf("major brand = %q, want iso5", brand)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("payload round-trip mismatch: %q", got)
	}
}

func TestDecodeDeterministic(t *testing.T) {
	// Same input must produce byte-identical containers across calls.
	a, err := Encode([]byte("x"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := Encode([]byte("x"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b) {
		t.Fatal("encoding is not deterministic")
	}
}

func TestDecodeRejectsCorruption(t *testing.T) {
	raw, err := Encode([]byte("payload-bytes"))
	if err != nil {
		t.Fatal(err)
	}
	// Truncate in the middle of a box header/body: structurally invalid.
	truncated := raw[:len(raw)/2]
	if _, _, err := Decode(truncated); diag.CodeOf(err) != diag.EInputCorrupt {
		t.Fatalf("truncated container must be E_INPUT_CORRUPT, got %v", err)
	}
	// A garbage stream is likewise rejected.
	if _, _, err := Decode([]byte("not an mp4 at all")); diag.CodeOf(err) != diag.EInputCorrupt {
		t.Fatalf("garbage must be E_INPUT_CORRUPT, got %v", err)
	}
}
