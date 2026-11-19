package remux

import (
	"testing"

	"archive/internal/errs"
	"archive/internal/mp4"
)

func segment(t *testing.T, baseDTS, audioBase int64, tfdt32 bool, payloadPrefix string) []mp4.Track {
	t.Helper()
	ws := &mp4.MemWS{}
	m, err := mp4.NewMuxer(ws, []mp4.MuxTrack{
		{ID: 1, Type: "video", Codec: "avc1", Timescale: 90000, Language: "eng"},
		{ID: 2, Type: "audio", Codec: "mp4a", Timescale: 48000, Language: "jpn"},
	})
	if err != nil {
		t.Fatal(err)
	}
	m.Tfdt32 = tfdt32
	err = m.AddFragment([]mp4.FragTrack{
		{Samples: []mp4.Sample{
			{DTS: baseDTS, Duration: 3000, Keyframe: true, Data: []byte(payloadPrefix + "v0")},
			{DTS: baseDTS + 3000, Duration: 3000, Data: []byte(payloadPrefix + "v1")},
		}},
		{Samples: []mp4.Sample{
			{DTS: audioBase, Duration: 1600, Data: []byte(payloadPrefix + "a0")},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	tracks, err := mp4.Demux("a", "seg.m4s", ws.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	return tracks
}

func TestRemuxRoundTrip(t *testing.T) {
	// two segments, the second continuing after a 32-bit wrap
	segs := [][]mp4.Track{
		segment(t, 0xfffff000, 0, true, "s0-"),
		segment(t, 0x1000, 1600, true, "s1-"),
	}
	out := &mp4.MemWS{}
	res, err := Remux("a", segs, out, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Samples != 6 || len(res.Tracks) != 2 {
		t.Fatalf("result %+v", res)
	}
	// re-demux the output: timestamps must be monotonic and unwrapped
	tracks, err := mp4.Demux("a", "out.m4s", out.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	var video *mp4.Track
	for i := range tracks {
		if tracks[i].Type == "video" {
			video = &tracks[i]
		}
	}
	if video == nil || len(video.Samples) != 4 {
		t.Fatalf("video %+v", video)
	}
	prev := int64(-1)
	for _, s := range video.Samples {
		if s.DTS <= prev {
			t.Fatalf("non-monotonic output DTS %d after %d", s.DTS, prev)
		}
		prev = s.DTS
	}
	// payloads survive losslessly
	if string(video.Samples[0].Data) != "s0-v0" || string(video.Samples[3].Data) != "s1-v1" {
		t.Fatalf("payloads corrupted: %q %q", video.Samples[0].Data, video.Samples[3].Data)
	}
	if video.Timescale != 90000 {
		t.Fatalf("timescale %d", video.Timescale)
	}
}

func TestRemuxRejectsNonMonotonic(t *testing.T) {
	good := segment(t, 0, 0, false, "ok-")
	bad := segment(t, 0, 0, false, "bad-")
	// corrupt the second segment: DTS jumps backwards
	for i := range bad {
		for j := range bad[i].Samples {
			bad[i].Samples[j].DTS = -1000
		}
	}
	out := &mp4.MemWS{}
	_, err := Remux("a", [][]mp4.Track{good, bad}, out, Options{})
	if err == nil {
		t.Fatal("non-monotonic track accepted")
	}
	if e, ok := err.(*errs.Error); !ok || e.Kind != errs.KindClock {
		t.Fatalf("expected KindClock, got %v", err)
	}
}

func TestRemuxRejectsEmpty(t *testing.T) {
	if _, err := Remux("a", nil, &mp4.MemWS{}, Options{}); err == nil {
		t.Fatal("empty input accepted")
	}
}
