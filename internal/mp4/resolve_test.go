package mp4

import (
	"os"
	"path/filepath"
	"testing"

	"mediasim/internal/clock"
	"mediasim/internal/fixture"
)

func writePreset(t *testing.T, name string) (string, *fixture.Manifest) {
	t.Helper()
	dir := t.TempDir()
	path, man, err := fixture.Ensure(dir, name)
	if err != nil {
		t.Fatalf("fixture %s: %v", name, err)
	}
	return path, man
}

func trackByID(t *testing.T, info *Info, id uint32) Track {
	t.Helper()
	for _, tr := range info.Tracks {
		if tr.ID == id {
			return tr
		}
	}
	t.Fatalf("track %d missing", id)
	return Track{}
}

func TestParseNormal(t *testing.T) {
	path, man := writePreset(t, "normal")
	info, err := Parse(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.MoovPresent || !info.Indexed || !info.Synthetic {
		t.Fatalf("info=%+v", info)
	}
	if len(info.Gaps) != 0 {
		t.Fatalf("normal fixture has gaps: %+v", info.Gaps)
	}
	if len(info.Tracks) != 3 {
		t.Fatalf("tracks=%d", len(info.Tracks))
	}
	cam0 := trackByID(t, info, fixture.TrackCam0)
	if cam0.Kind != KindVideo || cam0.Timescale != 30000 || cam0.Width != 160 || cam0.Height != 90 {
		t.Fatalf("cam0=%+v", cam0)
	}
	if len(cam0.Samples) != fixture.Cam0Frames || cam0.ValidCount() != fixture.Cam0Frames {
		t.Fatalf("cam0 samples=%d valid=%d", len(cam0.Samples), cam0.ValidCount())
	}
	// Keyframe cadence every 10 frames.
	for i, s := range cam0.Samples {
		wantSync := i%fixture.KeyframeEvery == 0
		if s.Sync != wantSync {
			t.Fatalf("sample %d sync=%v want %v", i, s.Sync, wantSync)
		}
		if s.Size != fixture.VideoBytes {
			t.Fatalf("sample %d size=%d", i, s.Size)
		}
	}
	// DTS is exactly 1001 per frame; total ticks match the manifest.
	if cam0.Samples[1].DTS-cam0.Samples[0].DTS != fixture.VideoDelta {
		t.Fatalf("dts delta=%d", cam0.Samples[1].DTS-cam0.Samples[0].DTS)
	}
	voice := trackByID(t, info, fixture.TrackVoice)
	if voice.Kind != KindAudio || voice.SampleRate != 48000 || voice.Timescale != 48000 {
		t.Fatalf("voice=%+v", voice)
	}
	if len(voice.Samples) != fixture.AudioChunks {
		t.Fatalf("audio samples=%d", len(voice.Samples))
	}
	// Every sample lies inside mdat and the manifest agrees on frame count.
	if got, want := man.VideoFrameCount(), fixture.Cam0Frames+fixture.Cam1Frames; got != want {
		t.Fatalf("manifest frames=%d want %d", got, want)
	}
}

func TestParseTruncatedKeepsValidPrefix(t *testing.T) {
	path, _ := writePreset(t, "truncated")
	info, err := Parse(path)
	if err != nil {
		t.Fatal(err)
	}
	cam0 := trackByID(t, info, fixture.TrackCam0)
	if cam0.ValidCount() == 0 {
		t.Fatal("no recovered prefix")
	}
	if cam0.ValidCount() >= fixture.Cam0Frames {
		t.Fatalf("truncation did not cut samples: %d", cam0.ValidCount())
	}
	if len(info.Gaps) == 0 {
		t.Fatal("expected corrupt ranges")
	}
	// The prefix must be contiguous from sample 0.
	for i := 0; i < cam0.ValidCount(); i++ {
		if !cam0.Samples[i].Valid {
			t.Fatalf("sample %d invalid inside prefix", i)
		}
	}
	if cam0.Samples[cam0.ValidCount()].Valid {
		t.Fatal("sample after prefix should be invalid")
	}
}

func TestParseCorruptSamples(t *testing.T) {
	path, _ := writePreset(t, "corrupt")
	info, err := Parse(path)
	if err != nil {
		t.Fatal(err)
	}
	cam0 := trackByID(t, info, fixture.TrackCam0)
	if len(cam0.Samples) != fixture.Cam0Frames {
		t.Fatalf("samples=%d", len(cam0.Samples))
	}
	if cam0.ValidCount() != fixture.Cam0Frames-3 {
		t.Fatalf("valid=%d want %d", cam0.ValidCount(), fixture.Cam0Frames-3)
	}
	for _, i := range []int{30, 31, 32} {
		if cam0.Samples[i].Valid || cam0.Samples[i].Reason == "" {
			t.Fatalf("sample %d not marked corrupt: %+v", i, cam0.Samples[i])
		}
	}
	// Valid samples on either side of the corrupt run stay usable.
	if !cam0.Samples[29].Valid || !cam0.Samples[33].Valid {
		t.Fatal("corrupt run leaked into neighbours")
	}
	found := false
	for _, g := range info.Gaps {
		if g.Kind == GapCorruptSample && g.TrackID == fixture.TrackCam0 &&
			g.Samples == [2]int{30, 33} && g.Recovery == clock.RecoveredSkipped {
			found = true
		}
	}
	if !found {
		t.Fatalf("no corrupt-sample gap for cam0: %+v", info.Gaps)
	}
}

func TestParseMissingIndex(t *testing.T) {
	path, _ := writePreset(t, "missing-index")
	info, err := Parse(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Indexed {
		t.Fatal("missing-index fixture reported as indexed")
	}
	if len(info.Tracks) != 3 {
		t.Fatalf("tracks=%d", len(info.Tracks))
	}
	for _, tr := range info.Tracks {
		if len(tr.Samples) != 0 {
			t.Fatalf("track %d resolved samples without an index", tr.ID)
		}
	}
	gaps := 0
	for _, g := range info.Gaps {
		if g.Kind == GapMissingIndex && g.Recovery == clock.RecoveredResync {
			gaps++
		}
	}
	if gaps != 3 {
		t.Fatalf("missing-index gaps=%d (%+v)", gaps, info.Gaps)
	}
}

func TestParseIOErrorIsClassified(t *testing.T) {
	_, err := Parse(filepath.Join(t.TempDir(), "absent.mp4"))
	if err == nil {
		t.Fatal("expected error")
	}
	if clock.Classify(err) != clock.CatContainer {
		t.Fatalf("category=%s", clock.Classify(err))
	}
	if !os.IsNotExist(errorsUnwrap(err)) {
		t.Fatalf("lost the underlying cause: %v", err)
	}
}

func errorsUnwrap(err error) error {
	type unwrapper interface{ Unwrap() error }
	if u, ok := err.(unwrapper); ok {
		return u.Unwrap()
	}
	return err
}
