package mp4

import (
	"bytes"
	"testing"
)

func muxFixture(t *testing.T, tfdt32 bool, baseDTS int64) []byte {
	t.Helper()
	ws := &MemWS{}
	m, err := NewMuxer(ws, []MuxTrack{
		{ID: 1, Type: "video", Codec: "avc1", Timescale: 90000, Language: "eng", Width: 640, Height: 360},
		{ID: 2, Type: "audio", Codec: "mp4a", Timescale: 48000, Language: "jpn"},
		{ID: 3, Type: "subtitle", Codec: "wvtt", Timescale: 1000},
	})
	if err != nil {
		t.Fatal(err)
	}
	m.Tfdt32 = tfdt32
	frags := []FragTrack{
		{Samples: []Sample{
			{DTS: baseDTS, Duration: 3000, Keyframe: true, Data: []byte("video-frame-0")},
			{DTS: baseDTS + 3000, CTSOffset: 1500, Duration: 3000, Data: []byte("video-frame-1")},
		}},
		{Samples: []Sample{
			{DTS: 0, Duration: 1024, Data: []byte("aac0")},
			{DTS: 1024, Duration: 1024, Data: []byte("aac1")},
		}},
		{Samples: []Sample{
			{DTS: 0, Duration: 2000, Data: []byte("WEBVTT cue")},
		}},
	}
	if err := m.AddFragment(frags); err != nil {
		t.Fatal(err)
	}
	return ws.buf
}

func TestMuxDemuxRoundTrip(t *testing.T) {
	data := muxFixture(t, false, 0)
	tracks, err := Demux("a", "seg.m4s", data)
	if err != nil {
		t.Fatal(err)
	}
	if len(tracks) != 3 {
		t.Fatalf("tracks %d", len(tracks))
	}
	v, a, s := tracks[0], tracks[1], tracks[2]
	if v.Type != "video" || v.Codec != "avc1" || v.Timescale != 90000 || v.Language != "eng" {
		t.Fatalf("video track %+v", v)
	}
	if a.Type != "audio" || a.Language != "jpn" || a.Timescale != 48000 {
		t.Fatalf("audio track %+v", a)
	}
	if s.Type != "subtitle" || s.Codec != "wvtt" {
		t.Fatalf("subtitle track %+v", s)
	}
	if len(v.Samples) != 2 || string(v.Samples[0].Data) != "video-frame-0" {
		t.Fatalf("video samples %+v", v.Samples)
	}
	if v.Samples[1].CTSOffset != 1500 || v.Samples[1].Keyframe {
		t.Fatalf("sample %+v", v.Samples[1])
	}
	if v.Samples[1].DTS != 3000 || a.Samples[1].DTS != 1024 {
		t.Fatalf("DTS v=%d a=%d", v.Samples[1].DTS, a.Samples[1].DTS)
	}
	if string(s.Samples[0].Data) != "WEBVTT cue" {
		t.Fatalf("subtitle payload %q", s.Samples[0].Data)
	}
}

func TestDemuxTruncated(t *testing.T) {
	data := muxFixture(t, false, 0)
	if _, err := Demux("a", "seg.m4s", data[:len(data)/2]); err == nil {
		t.Fatal("truncated segment accepted")
	}
}

func TestDemuxEmpty(t *testing.T) {
	if _, err := Demux("a", "seg.m4s", nil); err == nil {
		t.Fatal("empty segment accepted")
	}
}

func TestDemuxWrappedTfdt(t *testing.T) {
	data := muxFixture(t, true, 0xfffffc18)
	tracks, err := Demux("a", "seg.m4s", data)
	if err != nil {
		t.Fatal(err)
	}
	v := tracks[0]
	if v.ClockBits != 32 {
		t.Fatalf("clock bits %d", v.ClockBits)
	}
	// second sample crossed the 32-bit boundary: 0xfffffc18+3000 mod 2^32
	if v.Samples[1].DTS != 0x7d0 {
		t.Fatalf("wrapped DTS %#x", v.Samples[1].DTS)
	}
}

func TestMemWS(t *testing.T) {
	ws := &MemWS{}
	ws.Write([]byte("hello"))
	ws.Seek(0, 0)
	ws.Write([]byte("HE"))
	if !bytes.Equal(ws.buf, []byte("HEllo")) {
		t.Fatalf("buf %q", ws.buf)
	}
}
