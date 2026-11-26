package media

import (
	"bytes"
	"testing"

	"medcheck/fault"
)

func TestScanReaderResyncsAfterTruncatedHeader(t *testing.T) {
	first := EncodeHeader(AccessUnit{Index: 1, GOPID: 1, KeyFrame: true})
	second := EncodeHeader(AccessUnit{Index: 2, GOPID: 1})
	third := EncodeHeader(AccessUnit{Index: 3, GOPID: 2, KeyFrame: true})
	blob := append([]byte{}, first...)
	blob = append(blob, second[:16]...)
	blob = append(blob, third...)

	var units []AccessUnit
	var bad []HeaderError
	err := ScanReader(bytes.NewReader(blob), func(u AccessUnit, he *HeaderError) error {
		if he != nil {
			bad = append(bad, *he)
		} else {
			units = append(units, u)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(bad) != 1 || bad[0].Kind != fault.TruncatedHeader {
		t.Fatalf("bad=%+v", bad)
	}
	if len(units) != 2 || units[0].Index != 1 || units[1].Index != 3 {
		t.Fatalf("stream scanner swallowed later unit: %+v", units)
	}
}

func TestScanReaderSkipsPayloadAndContinues(t *testing.T) {
	var blob []byte
	for i := uint32(0); i < 3; i++ {
		u := AccessUnit{Index: i, GOPID: 0, PayloadLen: 5, KeyFrame: i == 0}
		blob = append(blob, EncodeHeader(u)...)
		blob = append(blob, []byte("media")...)
	}
	var got []uint32
	err := ScanReader(bytes.NewReader(blob), func(u AccessUnit, he *HeaderError) error {
		if he != nil {
			t.Fatalf("unexpected header error: %v", he)
		}
		got = append(got, u.Index)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[2] != 2 {
		t.Fatalf("payload skip lost units: %v", got)
	}
}
