package remux

import (
	"bytes"
	"testing"
)

func TestQualitySidecarRoundTrip(t *testing.T) {
	evidence := []byte(`{"payload_sha256_match":true,"quality_blocks":120}`)
	brand, got, err := RoundTripQualitySidecar(evidence)
	if err != nil {
		t.Fatalf("sidecar round trip: %v", err)
	}
	if brand != "isom" {
		t.Fatalf("ftyp major brand wrong: %q", brand)
	}
	if !bytes.Equal(got, evidence) {
		t.Fatalf("evidence payload altered: got %q want %q", got, evidence)
	}
}
