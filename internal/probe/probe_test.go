package probe_test

import (
	"path/filepath"
	"testing"

	"mediac/internal/fixture"
	"mediac/internal/model"
	"mediac/internal/probe"
)

const frames = 100 // 4 s of 25 fps video, 200 audio frames

func writeGood(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if _, err := fixture.WriteFile(dir, "good.mp4", fixture.Standard(1, frames)); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, "good.mp4")
}

func TestProbeGood(t *testing.T) {
	res, merr := probe.Probe(writeGood(t))
	if merr != nil {
		t.Fatalf("good file reported error: %v", merr)
	}
	v := res.Track("video")
	if v == nil || v.Timescale != 90000 || len(v.Samples) != frames {
		t.Fatalf("bad video track: %+v", v)
	}
	if !v.Samples[0].Keyframe || v.Samples[1].Keyframe {
		t.Fatal("keyframe flags wrong")
	}
	a := res.Track("audio")
	if a == nil || a.Channels != 2 || a.SampleRate != 48000 || len(a.Samples) != 2*frames {
		t.Fatalf("bad audio track: %+v", a)
	}
	if v.Color == nil || v.Color.Primaries != 1 {
		t.Fatalf("color metadata lost: %+v", v.Color)
	}
	if ms, ok := v.Duration().MillisExact(); !ok || ms != 4000 {
		t.Fatalf("duration = %v ms (exact=%v), want 4000", ms, ok)
	}
}

func TestProbeTruncatedMoov(t *testing.T) {
	dir := t.TempDir()
	spec := fixture.Standard(1, frames)
	// Cut 50 bytes before the end of the file: inside the audio trak,
	// after the video sample tables have been fully parsed.
	spec.MoovTruncAt = moovLen(spec) - 50
	if _, err := fixture.WriteFile(dir, "trunc.mp4", spec); err != nil {
		t.Fatal(err)
	}
	res, merr := probe.Probe(filepath.Join(dir, "trunc.mp4"))
	if merr == nil || res == nil || res.Damage == nil {
		t.Fatalf("expected damage, got res=%v err=%v", res, merr)
	}
	if merr.OffsetBytes != res.Size {
		t.Fatalf("damaged_offset_bytes = %d, want file size %d", merr.OffsetBytes, res.Size)
	}
	// Media time must be the video duration (4000 ms) within one frame.
	if d := abs64(merr.MediaTimeMs - 4000); d > 40 {
		t.Fatalf("damaged_time_ms = %d, want 4000±40", merr.MediaTimeMs)
	}
	if merr.Exit != model.ExitDamagedInput {
		t.Fatalf("exit = %d, want 2", merr.Exit)
	}
}

// moovLen returns the byte length of the moov box of a spec.
func moovLen(spec fixture.Spec) int {
	full := fixture.Build(spec)
	// ftyp size + mdat size prefix the moov box; scan for "moov".
	for i := 0; i+8 <= len(full); i++ {
		if string(full[i+4:i+8]) == "moov" {
			return len(full) - i
		}
	}
	return 0
}

func TestProbeCorruptMdat(t *testing.T) {
	dir := t.TempDir()
	spec := fixture.Standard(1, frames)
	spec.MdatShortfall = 500 // last audio samples declare data that is missing
	if _, err := fixture.WriteFile(dir, "corrupt.mp4", spec); err != nil {
		t.Fatal(err)
	}
	res, merr := probe.Probe(filepath.Join(dir, "corrupt.mp4"))
	if merr == nil || res.Damage == nil || res.Damage.BoxType != "mdat" {
		t.Fatalf("expected mdat damage, got %+v err=%v", res.Damage, merr)
	}
	// First damaged audio sample: 200 samples of 300 bytes, 500 missing.
	// Sample i damaged when (i+1)*300 > 200*300-500 → i >= 199 (60000-500=59500; i=199: 60000>59500).
	wantIdx := 198 // (198+1)*300 = 59700 > 59500
	if got := res.Damage.MediaTime.Value * 1000 / res.Damage.MediaTime.Timescale; got != int64(wantIdx)*20 {
		t.Fatalf("damaged media time = %d ms, want %d ms", got, wantIdx*20)
	}
	if merr.MediaTimeMs != int64(wantIdx)*20 {
		t.Fatalf("damaged_time_ms = %d, want %d", merr.MediaTimeMs, wantIdx*20)
	}
}

func TestProbeNonMonotonic(t *testing.T) {
	dir := t.TempDir()
	spec := fixture.Standard(1, frames)
	off := make([]int32, frames)
	off[3] = -36000 // sample 3 presents before sample 2
	spec.Tracks[0].CttsOffsets = off
	if _, err := fixture.WriteFile(dir, "nonmono.mp4", spec); err != nil {
		t.Fatal(err)
	}
	res, merr := probe.Probe(filepath.Join(dir, "nonmono.mp4"))
	if merr == nil || res.Damage == nil || res.Damage.BoxType != "ctts" {
		t.Fatalf("expected ctts damage, got %+v err=%v", res.Damage, merr)
	}
	if merr.MediaTimeMs != 120 { // sample 3 DTS = 3*3600/90000 s
		t.Fatalf("damaged_time_ms = %d, want 120", merr.MediaTimeMs)
	}
}

func TestProbeUnknownCodec(t *testing.T) {
	dir := t.TempDir()
	spec := fixture.Standard(1, frames)
	spec.Tracks[0].Codec = "xyz1"
	if _, err := fixture.WriteFile(dir, "unknown.mp4", spec); err != nil {
		t.Fatal(err)
	}
	_, merr := probe.Probe(filepath.Join(dir, "unknown.mp4"))
	if merr == nil || merr.Code != "unknown_codec" {
		t.Fatalf("expected explicit unknown_codec failure, got %v", merr)
	}
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}
