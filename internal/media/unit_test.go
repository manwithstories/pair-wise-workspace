package media

import (
	"errors"
	"testing"

	"medcheck/fault"
)

func TestAccessUnitHeaderRoundTrip(t *testing.T) {
	u := AccessUnit{Index: 11, PTSMS: 440, GOPID: 2, IndexInGOP: 3, PayloadLen: 9, KeyFrame: true}
	h := EncodeHeader(u)
	got, err := DecodeHeader(h)
	if err != nil {
		t.Fatal(err)
	}
	if got.Index != u.Index || got.PTSMS != u.PTSMS || !got.KeyFrame || !got.CRCValid {
		t.Fatalf("round trip mismatch: %+v", got)
	}
}

func TestBadCRCIsLocalized(t *testing.T) {
	h := EncodeHeader(AccessUnit{Index: 1, PayloadLen: 4})
	h[10] ^= 0xff
	if _, err := DecodeHeader(h); !errors.Is(err, ErrBadCRC) {
		t.Fatalf("expected bad CRC, got %v", err)
	}
	_, bad := ScanHeaders(h)
	if len(bad) != 1 || bad[0].Kind != fault.BadCRC {
		t.Fatalf("expected one bad CRC range, got %+v", bad)
	}
}

func TestTruncatedHeaderDoesNotSwallowFollowingUnit(t *testing.T) {
	first := EncodeHeader(AccessUnit{Index: 1, GOPID: 1, PayloadLen: 0, KeyFrame: true})
	second := EncodeHeader(AccessUnit{Index: 2, GOPID: 1, PayloadLen: 0})
	third := EncodeHeader(AccessUnit{Index: 3, GOPID: 2, PayloadLen: 0, KeyFrame: true})

	// Replace the middle header with a 16-byte truncation, then concatenate
	// the intact third unit immediately after it.
	blob := append([]byte{}, first...)
	blob = append(blob, second[:16]...)
	blob = append(blob, third...)

	units, bad := ScanHeaders(blob)
	if len(bad) != 1 || bad[0].Kind != fault.TruncatedHeader {
		t.Fatalf("expected one truncated header, got units=%+v bad=%+v", units, bad)
	}
	if len(units) != 2 || units[0].Index != 1 || units[1].Index != 3 {
		t.Fatalf("truncated header swallowed later unit: %+v", units)
	}
}

func TestSizeLimit(t *testing.T) {
	if err := CheckSize(MaxInputBytes, MaxInputBytes); err != nil {
		t.Fatal(err)
	}
	if err := CheckSize(MaxInputBytes+1, MaxInputBytes); err == nil {
		t.Fatal("expected input above 2 GiB to be rejected")
	}
}
