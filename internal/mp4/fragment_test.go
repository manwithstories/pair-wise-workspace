package mp4

import (
	"bytes"
	"encoding/binary"
	"testing"

	"mediad/internal/core"
)

func testFragment() *Fragment {
	samples := make([]Sample, 6)
	for i := range samples {
		flags := uint32(0x01010000) // non-sync + standard depends
		cts := int32((i%3 - 1) * 512)
		if i == 0 {
			flags = 0x02000000 // sync sample, non_sync bit clear
			cts = 0
		}
		samples[i] = Sample{Duration: 3000, Size: 100 + uint32(i)*7, Flags: flags, CTOffset: cts}
	}
	var data bytes.Buffer
	for _, s := range samples {
		data.Write(make([]byte, int(s.Size)))
	}
	return &Fragment{
		Sequence:    7,
		TrackID:     1,
		BaseDecode:  90000 * 6,
		TFDTVersion: 1,
		Samples:     samples,
		Data:        data.Bytes(),
	}
}

func TestFragmentRoundTripIdentical(t *testing.T) {
	orig := testFragment()
	b, err := EncodeFragment(orig)
	if err != nil {
		t.Fatal(err)
	}
	f, out, err := RoundTrip(b)
	if err != nil {
		t.Fatalf("roundtrip parse: %v", err)
	}
	if f.Sequence != 7 || f.TrackID != 1 || f.BaseDecode != 540000 {
		t.Fatalf("header fields mismatch: %+v", f)
	}
	if len(f.Samples) != 6 || f.KeyframeCount() != 1 {
		t.Fatalf("samples/keyframes mismatch: n=%d kf=%d", len(f.Samples), f.KeyframeCount())
	}
	if f.Samples[2].CTOffset != orig.Samples[2].CTOffset {
		t.Fatalf("cts not preserved: %d != %d", f.Samples[2].CTOffset, orig.Samples[2].CTOffset)
	}
	if !bytes.Equal(b, out) {
		t.Fatalf("re-encoding is not byte identical:\nfirst=%x\nsecond=%x", b, out)
	}
}

func TestFragmentRoundTripTFDTV0(t *testing.T) {
	orig := testFragment()
	orig.TFDTVersion = 0
	orig.BaseDecode = 12345
	for i := range orig.Samples {
		orig.Samples[i].CTOffset = 100 // v0 non-negative
	}
	b, err := EncodeFragment(orig)
	if err != nil {
		t.Fatal(err)
	}
	_, out, err := RoundTrip(b)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(b, out) {
		t.Fatal("tfdt v0 roundtrip not identical")
	}
}

func TestParseTruncatedFragment(t *testing.T) {
	b, err := EncodeFragment(testFragment())
	if err != nil {
		t.Fatal(err)
	}
	cut := b[:len(b)-24] // slice through mdat -> declared box runs past EOF
	_, err = ParseFragment(cut)
	f, ok := core.AsFault(err)
	if !ok || (f.Kind != core.KindTruncated && f.Kind != core.KindOutOfBounds) {
		t.Fatalf("want truncation/oob fault, got %v", err)
	}
}

func TestParseAbsurdTrunCount(t *testing.T) {
	b, err := EncodeFragment(testFragment())
	if err != nil {
		t.Fatal(err)
	}
	moof, err := ReadHeader(b, 0)
	if err != nil {
		t.Fatal(err)
	}
	traf, _, err := FindDirectChild(b, moof, BoxTraf)
	if err != nil {
		t.Fatal(err)
	}
	trun, _, err := FindDirectChild(b, traf, BoxTrun)
	if err != nil {
		t.Fatal(err)
	}
	countOff := trun.Offset + 8 + 4 // header + fullbox
	binary.BigEndian.PutUint32(b[countOff:countOff+4], 0xFFFFFFFF)
	_, err = ParseFragment(b)
	f, ok := core.AsFault(err)
	if !ok || f.Kind != core.KindOutOfBounds {
		t.Fatalf("want oob fault for absurd count, got %v", err)
	}
}

func TestParseIllegalMfhdLength(t *testing.T) {
	b, err := EncodeFragment(testFragment())
	if err != nil {
		t.Fatal(err)
	}
	moof, err := ReadHeader(b, 0)
	if err != nil {
		t.Fatal(err)
	}
	mfhd, _, err := FindDirectChild(b, moof, BoxMfhd)
	if err != nil {
		t.Fatal(err)
	}
	binary.BigEndian.PutUint32(b[mfhd.Offset:mfhd.Offset+4], 4)
	_, err = ParseFragment(b)
	f, ok := core.AsFault(err)
	if !ok || f.Kind != core.KindBoxLength || f.Offset != mfhd.Offset {
		t.Fatalf("want box_length at %d, got %v", mfhd.Offset, err)
	}
}
