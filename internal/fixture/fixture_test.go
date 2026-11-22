package fixture

import (
	"bytes"
	"testing"
)

func TestMP4Deterministic(t *testing.T) {
	spec := []TrackSpec{{
		Kind: "video", Timescale: 30000, SampleDelta: 1001,
		SampleSizes: []uint32{100, 100, 100}, SyncSamples: []int{1}, CorruptSizeIdx: -1,
	}}
	a, err := MP4(spec)
	if err != nil {
		t.Fatal(err)
	}
	b, err := MP4(spec)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b) {
		t.Fatal("fixture generation is not deterministic")
	}
	if !bytes.Contains(a, []byte("ftyp")) || !bytes.Contains(a, []byte("moov")) || !bytes.Contains(a, []byte("mdat")) {
		t.Error("fixture missing required top-level boxes")
	}
}

func TestMP4KeyframeMarkers(t *testing.T) {
	spec := []TrackSpec{{
		Kind: "video", Timescale: 30000, SampleDelta: 1001,
		SampleSizes: []uint32{8, 8, 8, 8},
		SyncSamples: []int{1}, KeyframeMarkers: []int{1, 3}, CorruptSizeIdx: -1,
	}}
	data, err := MP4(spec)
	if err != nil {
		t.Fatal(err)
	}
	mdat := bytes.Index(data, []byte("mdat"))
	if mdat < 0 {
		t.Fatal("no mdat")
	}
	// "mdat" type tag is at mdat; payload starts 4 bytes later.
	samples := data[mdat+4:]
	if samples[0]&0x80 == 0 {
		t.Error("sample 1 marker missing")
	}
	if samples[8]&0x80 != 0 {
		t.Error("sample 2 should not be marked")
	}
	if samples[16]&0x80 == 0 {
		t.Error("sample 3 marker missing")
	}
}

func TestSRTAndVTTShape(t *testing.T) {
	srt := SRT(3, 0, 1_000_000_000, 500_000_000)
	if b := bytes.Count([]byte(srt), []byte("-->")); b != 3 {
		t.Errorf("srt arrows = %d, want 3", b)
	}
	vtt := VTT(3, 0, 1_000_000_000, 500_000_000)
	if !bytes.HasPrefix([]byte(vtt), []byte("WEBVTT")) {
		t.Error("vtt missing header")
	}
	if b := bytes.Count([]byte(vtt), []byte("cue-")); b != 3 {
		t.Errorf("vtt identifiers = %d, want 3", b)
	}
}

func TestRTPPacketRoundtrip(t *testing.T) {
	b := RTPPacket(7, 9000, 160)
	if len(b) != 12+160 {
		t.Errorf("packet len = %d, want %d", len(b), 12+160)
	}
}
