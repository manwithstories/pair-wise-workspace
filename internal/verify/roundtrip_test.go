package verify_test

import (
	"testing"

	"mediaproc/internal/verify"
)

func TestBuildAndValidateMP4Sidecar(t *testing.T) {
	data, err := verify.BuildMP4Sidecar(0, 200_000_000, []byte("hash-reference"))
	if err != nil {
		t.Fatal(err)
	}
	if len(data) < 32 {
		t.Fatalf("sidecar too short: %d", len(data))
	}
	if err := verify.ValidateMP4Sidecar(data); err != nil {
		t.Fatal(err)
	}
}

func TestRoundtripRecords(t *testing.T) {
	records := []verify.Record{{SegmentIndex: 0, SHA256: "abc", BlockCount: 3}}
	out, err := verify.RoundtripRecords(records)
	if err != nil {
		t.Fatal(err)
	}
	if out[0].SHA256 != "abc" || out[0].BlockCount != 3 {
		t.Fatalf("roundtrip changed record: %+v", out[0])
	}
	if got := verify.RoundtripPercent(5, 5); got != "100.00%" {
		t.Fatalf("percent=%q", got)
	}
}
