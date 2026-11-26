package verify

import (
	"path/filepath"
	"testing"

	"mediad/internal/media"
	"mediad/internal/mp4"
)

func TestVerifyRepairedProjectBoundaries(t *testing.T) {
	video := &media.Track{ID: 1, Kind: media.KindVideo, Timescale: 90_000, Width: 1280, Height: 720}
	for i := uint32(0); i < 30; i++ {
		video.Samples = append(video.Samples, media.Sample{
			Index: i + 1, PTS: int64(i) * 3000, DTS: int64(i) * 3000, Duration: 3000,
			Size: 32, Sync: i%10 == 0,
		})
	}
	audio := &media.Track{ID: 2, Kind: media.KindAudio, Timescale: 48_000, Audio: &media.AudioMetadata{SampleRate: 48_000, ChannelCount: 2}}
	for i := uint32(0); i < 47; i++ {
		audio.Samples = append(audio.Samples, media.Sample{
			Index: i + 1, PTS: int64(i) * 1024, DTS: int64(i) * 1024, Duration: 1024, Size: 16, Sync: true,
		})
	}
	p := &media.Project{MovieTimescale: 90_000, Tracks: []*media.Track{video, audio}}
	path := filepath.Join(t.TempDir(), "repaired.mp4")
	if err := mp4.WriteFile(path, p); err != nil {
		t.Fatal(err)
	}
	rep, err := VerifyFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.OK {
		t.Fatalf("report not ok: mismatch=%d misses=%d faults=%#v", rep.BoundaryMismatch, rep.IndexSeekMiss, rep.Faults)
	}
	if rep.OutputSHA256 == "" || len(rep.OutputSHA256) != 64 {
		t.Fatalf("sha=%q", rep.OutputSHA256)
	}
	if rep.IndexSeekMiss != 0 || rep.BoundaryMismatch != 0 {
		t.Fatalf("miss=%d mismatch=%d", rep.IndexSeekMiss, rep.BoundaryMismatch)
	}
}
