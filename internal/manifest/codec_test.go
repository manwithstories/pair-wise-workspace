package manifest

import (
	"bytes"
	"encoding/binary"
	"errors"
	"reflect"
	"testing"
)

func codecManifest() *Manifest {
	m := testManifest()
	m.Window = Window{Version: 4, StartNumber: 2, Size: 50}
	m.Representations[0].AudioOffsetUS = -37_000
	m.Representations[0].Segments = []Segment{
		{Number: 2, StartUS: 0, DurationUS: 4_000_000, RawStartUS: 1_100, DriftUS: 1_100, Truncated: true},
		{Number: 3, StartUS: 4_000_000, DurationUS: 4_000_000, RawStartUS: 3_999_100, DriftUS: -900, Discontinuity: true},
	}
	m.Representations[1].Segments = []Segment{{Number: 2, StartUS: 0, DurationUS: 4_000_000}}
	m.Discontinuities = []Discontinuity{{RepresentationID: "r1080", SegmentNumber: 3, RawStartUS: 3_999_100, GridStartUS: 4_000_000, DriftUS: -900, Direction: -1}}
	m.ResyncPoints = []ResyncPoint{{RepresentationID: "r1080", SegmentNumber: 3, GridStartUS: 4_000_000, DriftUS: -900, Direction: -1}}
	m.Stats = []RepresentationStats{{RepresentationID: "r1080", Bandwidth: 6_000_000, Samples: 2, TotalAbsUS: 2_000, MaxDriftUS: 1_100, P95DriftUS: 1_100, ResyncPoints: 1}}
	return m
}

func TestCodecRoundTripAndDeterminism(t *testing.T) {
	m := codecManifest()
	var first, second bytes.Buffer
	if err := Encode(m, &first); err != nil {
		t.Fatal(err)
	}
	if err := Encode(m, &second); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first.Bytes(), second.Bytes()) {
		t.Fatal("encoding is not deterministic")
	}
	decoded, err := Decode(bytes.NewReader(first.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(m, decoded) {
		t.Fatalf("round trip mismatch\nwant=%+v\ngot=%+v", m, decoded)
	}
}

func TestCodecRejectsCorruption(t *testing.T) {
	m := codecManifest()
	var buffer bytes.Buffer
	if err := Encode(m, &buffer); err != nil {
		t.Fatal(err)
	}
	data := buffer.Bytes()
	truncated := append([]byte(nil), data[:len(data)-1]...)
	if _, err := Decode(bytes.NewReader(truncated)); !errors.Is(err, ErrTruncated) {
		t.Fatalf("truncated error=%v", err)
	}
	corrupt := append([]byte(nil), data...)
	corrupt[len(corrupt)-1] ^= 0xff
	if _, err := Decode(bytes.NewReader(corrupt)); !errors.Is(err, ErrCRC) {
		t.Fatalf("crc error=%v", err)
	}
	version := append([]byte(nil), data...)
	version[4] = CodecVersion + 1
	if _, err := Decode(bytes.NewReader(version)); !errors.Is(err, ErrUnsupportedVersion) {
		t.Fatalf("version error=%v", err)
	}
	magicBytes := append([]byte(nil), data...)
	magicBytes[0] = 'X'
	if _, err := Decode(bytes.NewReader(magicBytes)); !errors.Is(err, ErrBadMagic) {
		t.Fatalf("magic error=%v", err)
	}
}

func TestCodecRejectsOversizeLengthPrefix(t *testing.T) {
	data := append([]byte(nil), magic[:]...)
	data = append(data, CodecVersion)
	data = binary.AppendUvarint(data, maxRecordSize+1)
	data = append(data, recordMeta)
	if _, err := Decode(bytes.NewReader(data)); !errors.Is(err, ErrLengthTooLarge) {
		t.Fatalf("length error=%v", err)
	}
}
