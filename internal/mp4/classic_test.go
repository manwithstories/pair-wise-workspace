package mp4

import "testing"

func TestClassicRoundTrip(t *testing.T) {
	tracks := []MuxTrack{
		{ID: 1, Type: "video", Codec: "avc1", Timescale: 90000, Language: "eng", Width: 640, Height: 360},
		{ID: 2, Type: "audio", Codec: "mp4a", Timescale: 48000, Language: "jpn"},
		{ID: 3, Type: "subtitle", Codec: "wvtt", Timescale: 1000},
	}
	samples := [][]Sample{
		{
			{DTS: 0, Duration: 3000, Keyframe: true, Data: []byte("v0-payload")},
			{DTS: 3000, CTSOffset: 1500, Duration: 3000, Data: []byte("v1-payload")},
			{DTS: 6000, Duration: 3000, Keyframe: true, Data: []byte("v2-payload")},
		},
		{
			{DTS: 0, Duration: 1024, Data: []byte("a0")},
			{DTS: 1024, Duration: 1024, Data: []byte("a1")},
		},
		{
			{DTS: 0, Duration: 2000, Data: []byte("WEBVTT cue")},
		},
	}
	ws := &MemWS{}
	if err := WriteClassic(ws, tracks, samples); err != nil {
		t.Fatal(err)
	}
	got, err := Demux("a", "movie.mp4", ws.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("tracks %d", len(got))
	}
	v := got[0]
	if v.Type != "video" || v.Codec != "avc1" || v.Timescale != 90000 || v.Language != "eng" {
		t.Fatalf("video meta %+v", v)
	}
	if len(v.Samples) != 3 {
		t.Fatalf("video samples %d", len(v.Samples))
	}
	if string(v.Samples[0].Data) != "v0-payload" || string(v.Samples[2].Data) != "v2-payload" {
		t.Fatalf("payloads %q %q", v.Samples[0].Data, v.Samples[2].Data)
	}
	if v.Samples[1].CTSOffset != 1500 {
		t.Fatalf("cto %d", v.Samples[1].CTSOffset)
	}
	if v.Samples[1].DTS != 3000 || v.Samples[0].Duration != 3000 {
		t.Fatalf("timing %+v", v.Samples)
	}
	if !v.Samples[0].Keyframe || v.Samples[1].Keyframe || !v.Samples[2].Keyframe {
		t.Fatalf("keyframes %+v", v.Samples)
	}
	a := got[1]
	if a.Type != "audio" || a.Language != "jpn" || len(a.Samples) != 2 {
		t.Fatalf("audio %+v", a)
	}
	if string(a.Samples[1].Data) != "a1" {
		t.Fatalf("audio payload %q", a.Samples[1].Data)
	}
	s := got[2]
	if s.Type != "subtitle" || len(s.Samples) != 1 || string(s.Samples[0].Data) != "WEBVTT cue" {
		t.Fatalf("subtitle %+v", s)
	}
}

func TestClassicTruncated(t *testing.T) {
	tracks := []MuxTrack{{ID: 1, Type: "video", Codec: "avc1", Timescale: 90000}}
	samples := [][]Sample{{{DTS: 0, Duration: 3000, Data: make([]byte, 4000)}}}
	ws := &MemWS{}
	if err := WriteClassic(ws, tracks, samples); err != nil {
		t.Fatal(err)
	}
	data := ws.Bytes()
	if _, err := Demux("a", "movie.mp4", data[:len(data)-100]); err == nil {
		t.Fatal("truncated classic MP4 accepted")
	}
}
