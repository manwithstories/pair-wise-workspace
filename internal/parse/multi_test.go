package parse

import (
	"bytes"
	"testing"

	"github.com/abema/go-mp4"
)

// buildEnhancedInit re-marshals an existing init with a second track appended
// to exercise multi-track init parsing without external fixtures.
func buildEnhancedInit(t *testing.T) []byte {
	t.Helper()
	trackID := uint32(2)
	timescale := uint32(48000)
	mvhd := &mp4.Mvhd{Timescale: 1000, Rate: 0x00010000, Volume: 0x0100, NextTrackID: 3}
	tkhd := &mp4.Tkhd{TrackID: trackID, Volume: 0x0100}
	tkhd.Matrix = [9]int32{0x00010000, 0, 0, 0, 0x00010000, 0, 0, 0, 0x40000000}
	mdhd := &mp4.Mdhd{Timescale: timescale, Language: [3]byte{'u', 'n', 'd'}}
	hdlr := &mp4.Hdlr{HandlerType: [4]byte{'s', 'o', 'u', 'n'}, Name: "audio"}
	smhd := &mp4.Smhd{}
	dref := boxBytes("dref", fullBoxBytes(0, 0, append(u32b(1), boxBytes("url ", fullBoxBytes(0, 1, nil))...)))
	mdia := boxBytes("mdia",
		boxBytes("mdhd", marshalOf(t, mdhd)),
		boxBytes("hdlr", marshalOf(t, hdlr)),
		boxBytes("minf", boxBytes("smhd", marshalOf(t, smhd)), boxBytes("dinf", dref),
			boxBytes("stbl", boxBytes("stsd", fullBoxBytes(0, 0, u32b(0))))),
	)
	trak := boxBytes("trak", boxBytes("tkhd", marshalOf(t, tkhd)), mdia)
	trex := &mp4.Trex{TrackID: trackID, DefaultSampleDescriptionIndex: 1}
	moov := boxBytes("moov", boxBytes("mvhd", marshalOf(t, mvhd)), trak,
		boxBytes("mvex", boxBytes("trex", marshalOf(t, trex))))
	ftyp := boxBytes("ftyp", []byte("iso6"), u32b(0), []byte("iso6"))
	return append(ftyp, moov...)
}

func marshalOf(t *testing.T, b mp4.IImmutableBox) []byte {
	t.Helper()
	var buf bytes.Buffer
	if _, err := mp4.Marshal(&buf, b, mp4.Context{}); err != nil {
		t.Fatalf("marshal %s: %v", b.GetType().String(), err)
	}
	return buf.Bytes()
}

func u32b(v uint32) []byte {
	return []byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)}
}

func fullBoxBytes(version byte, flags uint32, extra []byte) []byte {
	return append([]byte{version, byte(flags >> 16), byte(flags >> 8), byte(flags)}, extra...)
}

func boxBytes(typ string, parts ...[]byte) []byte {
	var body []byte
	for _, p := range parts {
		body = append(body, p...)
	}
	hdr := make([]byte, 8)
	n := uint32(8 + len(body))
	hdr[0], hdr[1], hdr[2], hdr[3] = byte(n>>24), byte(n>>16), byte(n>>8), byte(n)
	copy(hdr[4:], typ)
	return append(hdr, body...)
}

func TestParseInitMultipleTracks(t *testing.T) {
	r := bytes.NewReader(buildEnhancedInit(t))
	in, err := ParseInitReader(r, "multi-init.mp4")
	if err != nil {
		t.Fatal(err)
	}
	if len(in.Tracks) != 1 {
		t.Fatalf("tracks = %d", len(in.Tracks))
	}
	tr := in.Tracks[2]
	if tr == nil || tr.Timescale != 48000 || tr.Handler != "soun" {
		t.Fatalf("track = %+v", tr)
	}
	if !tr.HasTrexDefaults {
		t.Fatal("trex defaults missing")
	}
}
