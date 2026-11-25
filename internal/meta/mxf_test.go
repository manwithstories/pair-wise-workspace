package meta

import (
	"bytes"
	"testing"

	"relaycheck/internal/media"
)

func TestMXFOkAndConflict(t *testing.T) {
	m, f := ReadMXF(BuildMXFCase("ok"))
	if f != nil {
		t.Fatal(f)
	}
	if m.Track.ID != 1 || m.Track.TimeScale != 48000 || m.Track.Duration != 192000 {
		t.Fatalf("bad track: %+v", m.Track)
	}
	if m.Track.Rotation.Degrees != 90 || m.Track.Crop == nil || !m.Track.Crop.InBounds() {
		t.Fatalf("bad rotation/crop: %+v %+v", m.Track.Rotation, m.Track.Crop)
	}
	d, f := m.Reconcile(Layer{})
	if f != nil {
		t.Fatal(f)
	}
	if d.Tracks[0].Color.Primaries != "bt2020" || len(d.Conflicts) != 1 {
		t.Fatalf("want bt2020 track win + conflict, got %+v conflicts=%d", d.Tracks[0].Color, len(d.Conflicts))
	}
}

func TestMXFAnomalies(t *testing.T) {
	want := map[string]media.Code{
		"illegal-rotation": media.CodeInvalidRotation,
		"bad-crop":         media.CodeInvalidCrop,
		"unknown-color":    media.CodeUnknownColor,
	}
	for name, code := range want {
		m, f := ReadMXF(BuildMXFCase(name))
		if f != nil {
			t.Fatalf("%s read: %v", name, f)
		}
		_, f = m.Reconcile(Layer{})
		if f == nil || f.Code != code {
			t.Fatalf("%s code=%v want %s", name, f, code)
		}
	}
}

func TestMXFUnknownTagPreservedOrStrictError(t *testing.T) {
	b := BuildMXFCase("unknown-tag")
	m, f := ReadMXF(b)
	if f != nil {
		t.Fatal(f)
	}
	if len(m.Unknown) != 1 || string(m.Unknown[0].Value) != "preserve-me" {
		t.Fatalf("unknown tag not preserved: %+v", m.Unknown)
	}
	if _, f := ReadMXFStrict(b); f == nil || f.Code != media.CodeParseError {
		t.Fatalf("strict mode must reject unknown tag, got %v", f)
	}
}

func TestMXFTruncatedCarriesTrack(t *testing.T) {
	_, f := ReadMXF(BuildMXFCase("truncated-mxf"))
	if f == nil || f.Code != media.CodeParseError {
		t.Fatalf("want parse_error, got %v", f)
	}
	if f.TrackID != 1 {
		t.Fatalf("track=%d want 1", f.TrackID)
	}
}

func TestDecodeBERLength(t *testing.T) {
	cases := []struct {
		b    []byte
		want uint64
	}{
		{[]byte{0x20}, 32},
		{[]byte{0x81, 0x80}, 128},
		{[]byte{0x82, 0x01, 0x00}, 256},
	}
	for _, c := range cases {
		got, h, f := decodeBERLength(c.b, 0)
		if f != nil || got != c.want || h != len(c.b) {
			t.Fatalf("BER %x => %d,h=%d,%v want %d", c.b, got, h, f, c.want)
		}
	}
	if _, _, f := decodeBERLength([]byte{0x84, 0x01}, 0); f == nil {
		t.Fatal("truncated long BER must error")
	}
}

func TestMXFDeterministic(t *testing.T) {
	if !bytes.Equal(BuildMXFCase("ok"), BuildMXFCase("ok")) {
		t.Fatal("mxf fixture not deterministic")
	}
}
