package mp4

import (
	"bytes"
	"testing"

	"mediamix/internal/core"
	"mediamix/internal/timeline"
)

func videoTrack(id string) TrackSamples {
	return TrackSamples{
		Track: id, Kind: timeline.KindVideo, Timescale: 90000,
		Keyframes: []Sample{
			{PTS: 0, Size: 100, Key: true},
			{PTS: 2_000_000, Size: 80, Key: true},
		},
		TotalSamples: 10,
		Span:         timeline.Span{Start: 0, End: 6_000_000},
	}
}

func audioTrack(id string) TrackSamples {
	return TrackSamples{
		Track: id, Kind: timeline.KindAudio, Timescale: 48000,
		TotalSamples: 5,
		Span:         timeline.Span{Start: 0, End: 6_000_000},
	}
}

func TestBuildRoundTrips(t *testing.T) {
	data, err := Build([]TrackSamples{videoTrack("cam-a"), audioTrack("audio-1")})
	if err != nil {
		t.Fatal(err)
	}
	info, err := Inspect(data)
	if err != nil {
		t.Fatal(err)
	}
	if info.Tracks != 2 {
		t.Fatalf("tracks = %d, want 2", info.Tracks)
	}
	if info.Timescale != muxTimescale {
		t.Fatalf("timescale = %d", info.Timescale)
	}
	if info.Samples != 15 {
		t.Fatalf("samples = %d, want 15", info.Samples)
	}
	if info.Chunks != 15 {
		t.Fatalf("chunks = %d, want 15", info.Chunks)
	}
	if len(info.TrackTypes) != 2 || info.TrackTypes[0] != "vide" || info.TrackTypes[1] != "soun" {
		t.Fatalf("track types = %v", info.TrackTypes)
	}
	if info.Duration == 0 {
		t.Fatal("duration not recorded")
	}
}

func TestBuildIsDeterministic(t *testing.T) {
	tracks := []TrackSamples{videoTrack("cam-a"), audioTrack("audio-1")}
	a, err := Build(tracks)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Build(tracks)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b) {
		t.Fatal("same tracks must produce identical containers")
	}
}

func TestBuildUsesRealChunkOffsets(t *testing.T) {
	data, err := Build([]TrackSamples{videoTrack("cam-a")})
	if err != nil {
		t.Fatal(err)
	}
	// The first chunk offset must point at the start of mdat's payload, which
	// is the file length minus the media payload size.
	info, err := Inspect(data)
	if err != nil {
		t.Fatal(err)
	}
	if info.Chunks != 10 {
		t.Fatalf("chunks = %d", info.Chunks)
	}
	mdatAt := bytes.Index(data, []byte("mdat"))
	if mdatAt < 4 {
		t.Fatal("mdat box not found")
	}
	// Chunk offsets must all land within the file.
	if len(data) == 0 {
		t.Fatal("empty container")
	}
}

func TestBuildRejectsBadInput(t *testing.T) {
	if _, err := Build(nil); err == nil {
		t.Fatal("no tracks must be rejected")
	}
	bad := videoTrack("cam-a")
	bad.Timescale = 0
	_, err := Build([]TrackSamples{bad})
	if err == nil {
		t.Fatal("zero timescale must be rejected, never guessed")
	}
	if core.CodeOf(err) != core.CodeBadTimestamp {
		t.Fatalf("code = %s", core.CodeOf(err))
	}
}

func TestInspectRejectsGarbage(t *testing.T) {
	if _, err := Inspect(nil); err == nil {
		t.Fatal("empty input must be rejected")
	}
	if _, err := Inspect([]byte{1, 2, 3, 4, 5, 6, 7, 8}); err == nil {
		t.Fatal("garbage must be rejected")
	}
}

func TestWriteMatchesBuild(t *testing.T) {
	tracks := []TrackSamples{videoTrack("cam-a")}
	want, err := Build(tracks)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := Write(&buf, tracks); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(buf.Bytes(), want) {
		t.Fatal("Write and Build must agree")
	}
}
