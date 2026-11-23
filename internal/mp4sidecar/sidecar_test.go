package mp4sidecar

import (
	"os"
	"testing"

	"medpipe/internal/clock"
	"medpipe/internal/model"
)

// fixture builds a tiny VFR timeline with one video and one audio track.
func fixture(t *testing.T) (*clock.Timeline, map[string][]model.Frame) {
	t.Helper()
	tracks := []model.Track{
		{ID: "cam0", Kind: model.KindVideo, Timescale: 90000, ClockBits: 16, NominalTickDelta: 3000},
		{ID: "mic0", Kind: model.KindAudio, Timescale: 48000, SampleRate: 48000, ClockBits: 16, NominalTickDelta: 1024},
	}
	frames := map[string][]model.Frame{"cam0": {}, "mic0": {}}
	for i := 0; i < 8; i++ {
		frames["cam0"] = append(frames["cam0"], model.Frame{
			TrackID: "cam0", Seq: uint64(i), RawTS: uint64(60000+i*3000) % 65536,
			Keyframe: i%3 == 0, Width: 64, Height: 48,
			Payload: []byte{byte(i), 0xa, 0xb, 0xc},
		})
	}
	for i := 0; i < 5; i++ {
		frames["mic0"] = append(frames["mic0"], model.Frame{
			TrackID: "mic0", Seq: uint64(i), RawTS: uint64(64000+i*1024) % 65536,
			Energy: []float64{0.1, 0.5}, Payload: []byte{0x55, byte(i)},
		})
	}
	tl, err := clock.Build(tracks, frames, clock.Options{})
	if err != nil {
		t.Fatalf("build timeline: %v", err)
	}
	return tl, frames
}

func TestSidecarRoundTrip(t *testing.T) {
	tl, frames := fixture(t)
	f, err := os.CreateTemp(t.TempDir(), "sidecar-*.mp4")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := Write(f, tl, frames); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := f.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	sum, err := Read(f)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(sum.Tracks) != 2 {
		t.Fatalf("tracks = %d, want 2", len(sum.Tracks))
	}
	seen := map[string]TrackSummary{}
	for _, tr := range sum.Tracks {
		seen[tr.ID] = tr
	}
	cam, mic := seen["cam0"], seen["mic0"]
	if cam.SampleCount != 8 || cam.KeyframeCount != 3 {
		t.Errorf("cam0 samples=%d keyframes=%d, want 8/3", cam.SampleCount, cam.KeyframeCount)
	}
	if mic.SampleCount != 5 || mic.KeyframeCount != 0 {
		t.Errorf("mic0 samples=%d keyframes=%d, want 5/0", mic.SampleCount, mic.KeyframeCount)
	}
	wantBytes := 8*4 + 5*2
	if sum.PayloadBytes != wantBytes {
		t.Errorf("payload bytes = %d, want %d", sum.PayloadBytes, wantBytes)
	}
	if cam.Timescale != 90000 || mic.Timescale != 48000 {
		t.Errorf("timescales = %d/%d", cam.Timescale, mic.Timescale)
	}
}

func TestSidecarEmptyTrack(t *testing.T) {
	tl, frames := fixture(t)
	frames["mic0"] = nil
	// Rebuild so the audio timeline reflects the empty slice.
	var tracks []model.Track
	for i := range tl.Tracks {
		tracks = append(tracks, tl.Tracks[i].Track)
	}
	tl, err := clock.Build(tracks, frames, clock.Options{})
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.CreateTemp(t.TempDir(), "sidecar-*.mp4")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := Write(f, tl, frames); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := f.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	sum, err := Read(f)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	for _, tr := range sum.Tracks {
		if tr.ID == "mic0" && tr.SampleCount != 0 {
			t.Errorf("empty track sample count = %d", tr.SampleCount)
		}
	}
}
