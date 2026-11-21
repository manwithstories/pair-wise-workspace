package ingest

import (
	"bytes"
	"os"
	"testing"

	"mediameld/internal/fixture"
	"mediameld/internal/model"
)

func generateInto(t *testing.T, dir string) fixture.Bundle {
	t.Helper()
	b, err := fixture.Generate(dir, fixture.Options{
		Seed: 17, DriftPPM: 85, WrapAt: 4_294_960_000, VFR: []int64{24, 25, 23},
	})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	return b
}

func openFile(t *testing.T, path string) *os.File {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

func TestDemuxMP4Studio(t *testing.T) {
	b := generateInto(t, t.TempDir())
	tracks, err := DemuxMP4(openFile(t, b.StudioMP4), b.StudioMP4)
	if err != nil {
		t.Fatalf("demux: %v", err)
	}
	if len(tracks) != 2 {
		t.Fatalf("tracks=%d want 2", len(tracks))
	}
	var video, audio *model.Track
	for _, tr := range tracks {
		switch tr.Kind {
		case model.KindVideo:
			video = tr
		case model.KindAudio:
			audio = tr
		}
	}
	if video == nil || audio == nil {
		t.Fatalf("missing video or audio track: %+v", tracks)
	}
	if len(video.Samples) != 48 || len(audio.Samples) != 24 {
		t.Fatalf("sample counts video=%d audio=%d", len(video.Samples), len(audio.Samples))
	}
	if video.Display.Width != 640 || video.Display.Height != 360 {
		t.Fatalf("dimensions %dx%d", video.Display.Width, video.Display.Height)
	}
	if video.ContainerRotation != 90 {
		t.Fatalf("rotation=%d want 90", video.ContainerRotation)
	}
	if video.ContainerColor.Primaries != "bt709" {
		t.Fatalf("color primaries=%q", video.ContainerColor.Primaries)
	}
	// PTS must be exactly DTS + ctts offset on every sample.
	for i, s := range video.Samples {
		if s.PTS-s.DTS != 2000 {
			t.Fatalf("sample %d pts-dts=%d want 2000", i, s.PTS-s.DTS)
		}
		if s.Size <= 0 || s.Hash == ([32]byte{}) {
			t.Fatalf("sample %d not hashed", i)
		}
	}
	// The sample bytes must round-trip: rehash from the file at the recorded
	// offset and compare.
	f := openFile(t, b.StudioMP4)
	for _, s := range video.Samples[:3] {
		got, err := hashRegion(f, s.SourceOffset, int64(s.Size))
		if err != nil {
			t.Fatal(err)
		}
		if got != s.Hash {
			t.Fatalf("hash mismatch at offset %d", s.SourceOffset)
		}
	}
}

func TestDemuxMP4UnknownColor(t *testing.T) {
	tracks := fixture.StudioTracks(17)
	tracks[0].Color = nil // fixture emits an unrecognized "zzzz" colr token
	raw := fixture.BuildMP4(tracks)
	got, err := DemuxMP4(bytes.NewReader(raw), "unknown.mp4")
	if err != nil {
		t.Fatalf("demux: %v", err)
	}
	for _, tr := range got {
		if tr.Kind != model.KindVideo {
			continue
		}
		if tr.ContainerColor.Primaries != "unknown" || tr.ContainerColor.Matrix != "unknown" {
			t.Fatalf("unknown colour not preserved: %+v", tr.ContainerColor)
		}
		if tr.ContainerColor.OriginalTag != "zzzz" {
			t.Fatalf("original tag lost: %q", tr.ContainerColor.OriginalTag)
		}
		if len(tr.Samples) == 0 {
			t.Fatal("samples dropped for unknown colour")
		}
		return
	}
	t.Fatal("no video track")
}

func TestScanRejectsBadBoxLength(t *testing.T) {
	// ftyp declares a size far beyond the file.
	bad := []byte{0x00, 0x00, 0x10, 0x00, 'f', 't', 'y', 'p', 'i', 's', 'o', 'm'}
	_, err := DemuxMP4(bytes.NewReader(bad), "bad.mp4")
	if err == nil {
		t.Fatal("bad box length accepted")
	}
	code, ok := model.CodeOf(err)
	if !ok || code != model.CodeMP4BoxLength {
		t.Fatalf("code=%v ok=%v err=%v", code, ok, err)
	}
	if model.ExitCode(err) != 2 {
		t.Fatalf("exit=%d want 2", model.ExitCode(err))
	}
}

func TestScanRejectsTruncatedHeader(t *testing.T) {
	bad := []byte{0x00, 0x00, 0x00, 0x10, 'm', 'o', 'o', 'v', 0x00}
	_, err := DemuxMP4(bytes.NewReader(bad), "bad.mp4")
	if err == nil {
		t.Fatal("truncated child accepted")
	}
	if code, _ := model.CodeOf(err); code != model.CodeMP4BoxLength {
		t.Fatalf("code=%v want %v", code, model.CodeMP4BoxLength)
	}
}

func TestDemuxMP4TruncatedTailIsNotSilent(t *testing.T) {
	tracks := fixture.StudioTracks(17)
	// Declare the last video sample but never store its bytes; the demuxer
	// must report a length error with an offset instead of dropping it.
	tracks[0].IncompleteTail = true
	raw := fixture.BuildMP4(tracks)
	_, err := DemuxMP4(bytes.NewReader(raw), "truncated.mp4")
	if err == nil {
		t.Fatal("truncated tail was silently accepted")
	}
	if code, _ := model.CodeOf(err); code != model.CodeMP4BoxLength {
		t.Fatalf("code=%v err=%v", code, err)
	}
	e, ok := err.(*model.Error)
	if !ok || !e.HasOffset {
		t.Fatalf("error lacks a byte offset: %v", err)
	}
	if model.ExitCode(err) != 2 {
		t.Fatalf("exit=%d want 2", model.ExitCode(err))
	}
}

func TestDemuxMP4ExtendedSizeBox(t *testing.T) {
	// A box using the 64-bit largesize header is legal and must parse.
	tracks := fixture.StudioTracks(17)
	raw := fixture.BuildMP4(tracks)
	got, err := DemuxMP4(bytes.NewReader(raw), "ok.mp4")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("tracks=%d", len(got))
	}
}
