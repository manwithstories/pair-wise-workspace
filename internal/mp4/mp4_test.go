package mp4

import (
	"os"
	"path/filepath"
	"testing"

	gomp4 "github.com/abema/go-mp4"
)

func TestColorConflictUsesConfiguredPriorityAndPreservesSources(t *testing.T) {
	sources := []ColorMetadata{
		{Source: "a", Standard: ColorBT2020},
		{Source: "b", Standard: ColorBT709},
	}
	got, err := ResolveColor(sources, DefaultColorConfig())
	if err != nil {
		t.Fatal(err)
	}
	if !got.Conflict || got.Selected.Standard != ColorBT709 {
		t.Fatalf("decision conflict=%v selected=%s", got.Conflict, got.Selected.Standard)
	}
	if len(got.Sources) != 2 || got.Sources[0].Primaries != 9 || got.Sources[1].Primaries != 1 {
		t.Fatalf("source values not preserved/normalized: %+v", got.Sources)
	}

	cfg := ColorConfig{Priority: []ColorStandard{ColorBT2020, ColorBT709}}
	got, err = ResolveColor(sources, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got.Selected.Standard != ColorBT2020 || got.Selected.Transfer != 16 {
		t.Fatalf("priority override selected=%+v", got.Selected)
	}
}

func TestColorRejectsEmptyConfigAndIllegalMetadata(t *testing.T) {
	if _, err := ResolveColor([]ColorMetadata{{Standard: ColorBT709}}, ColorConfig{}); err == nil {
		t.Fatal("empty color config accepted")
	}
	bad := []ColorMetadata{{Standard: ColorBT709, Primaries: 9, Transfer: 1, Matrix: 1}}
	if _, err := ResolveColor(bad, DefaultColorConfig()); err == nil {
		t.Fatal("illegal BT.709 primaries accepted")
	}
}

func TestMuxAndValidateSampleTables(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.mp4")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	const audioSamples = 3
	audio := AudioTrack{SampleRate: 48000, Channels: 2, Bits: 16, Timescale: 48000}
	for i := 0; i < audioSamples; i++ {
		audio.Samples = append(audio.Samples, make([]byte, 960*4))
		audio.Durations = append(audio.Durations, 960)
	}
	video := VideoTrack{Width: 64, Height: 48, Timescale: 90000}
	for i := 0; i < 4; i++ {
		nal := []byte{0x41, 0x00, byte(i)}
		sync := i == 0
		if sync {
			nal = []byte{0x65, 0x00}
		}
		video.Samples = append(video.Samples, VideoSample{NALs: [][]byte{nal}, Delta: 3000, Sync: sync})
	}
	color, err := ResolveColor([]ColorMetadata{{Standard: ColorBT709}}, DefaultColorConfig())
	if err != nil {
		t.Fatal(err)
	}
	cfg := MuxConfig{Audio: audio, Video: video, Color: color}
	if _, err := Mux(f, cfg); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	got, err := ValidateFile(path, cfg)
	if err != nil {
		t.Fatal(err)
	}
	probeFile, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	probeInfo, err := gomp4.Probe(probeFile)
	probeFile.Close()
	if err != nil {
		t.Fatalf("go-mp4 probe rejected muxed boxes: %v", err)
	}
	if len(probeInfo.Tracks) != 2 || len(probeInfo.Tracks[0].Samples) != 3 || len(probeInfo.Tracks[1].Samples) != 4 {
		t.Fatalf("probe tracks=%+v", probeInfo.Tracks)
	}
	if got.AudioSamples != 3 || got.VideoSamples != 4 {
		t.Fatalf("sample counts audio=%d video=%d", got.AudioSamples, got.VideoSamples)
	}
	if got.AudioDuration != 2880 || got.VideoDuration != 12000 {
		t.Fatalf("durations audio=%d video=%d", got.AudioDuration, got.VideoDuration)
	}
	found := map[string]bool{}
	for _, b := range got.Boxes {
		found[b.Type] = true
		if b.Size < 8 || b.Offset+b.Size > uint64(fileSize(t, path)) {
			t.Fatalf("box boundary invalid: %+v", b)
		}
	}
	for _, typ := range []string{"ftyp", "mdat", "moov", "trak", "stsd", "stts", "stsz", "stco", "avc1", "colr", "ipcm"} {
		if !found[typ] {
			t.Fatalf("missing box %s; found=%v", typ, found)
		}
	}
}

func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return st.Size()
}
