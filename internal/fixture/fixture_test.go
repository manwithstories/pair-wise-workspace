package fixture

import (
	"path/filepath"
	"testing"

	"mediaaudit/internal/mp4"
	"mediaaudit/internal/wav"
)

func TestBuildFullPackageAndAnalyze(t *testing.T) {
	dir := t.TempDir()
	o := Options{
		Dir: dir, Seed: 7, Channels: 6, Frames: 48000, SampleRate: 48000,
		TileFrames: 32, DamageFrame: 8192, WrapAt: 4294967000,
		DriftPPM: 180, VFRPattern: []int{2, 3, 1},
	}
	m, err := Build(o)
	if err != nil {
		t.Fatal(err)
	}
	if m.DamageCount != 1 || m.Fingerprint == "" {
		t.Fatalf("manifest wrong: %+v", m)
	}

	// WAV: exactly one CRC fault on frames 8192..8223.
	rd, err := wav.NewReader(filepath.Join(dir, AudioFile))
	if err != nil {
		t.Fatal(err)
	}
	if rd.Header().Channels != 6 || rd.Header().FrameCount != 48000 {
		t.Fatalf("wav header wrong: %+v", rd.Header())
	}
	rep, err := rd.Inspect()
	if err != nil {
		t.Fatal(err)
	}
	if rep.Faults.Count() != 1 {
		t.Fatalf("wav faults=%d want 1: %+v", rep.Faults.Count(), rep.Faults.All())
	}
	f := rep.Faults.All()[0]
	if f.Range.First != 8192 || f.Range.Last != 8224 {
		t.Fatalf("damage range=%v want [8192,8224)", f.Range)
	}
	if f.ByteOffset < 0 {
		t.Fatal("damage must carry a source byte offset")
	}
	rd.Close()

	// RTP: wraps occur, drift ~180ppm, no undecidable normal steps.
	rf, err := openRTP(filepath.Join(dir, RTPFile))
	if err != nil {
		t.Fatal(err)
	}
	if rf.Faults().Count() != 0 {
		t.Fatalf("normal drift must not be undecidable: %+v", rf.Faults().All())
	}
	if len(rf.Observations()) < 100 {
		t.Fatal("expected many RTP observations")
	}
	drift, err := rf.EstimateDrift()
	if err != nil {
		t.Fatal(err)
	}
	if d := drift.PPM - 180; d < -25 || d > 25 {
		t.Fatalf("drift PPM=%.2f want ~180", drift.PPM)
	}

	// MP4: VFR table with variable keyframe gaps.
	mres, err := mp4.Scan(filepath.Join(dir, MP4File))
	if err != nil {
		t.Fatal(err)
	}
	tr := mres.Tracks[0]
	if !tr.IsVFR {
		t.Fatal("proxy must be VFR")
	}
	if len(tr.KeyframeGapsMs) == 0 {
		t.Fatal("expected keyframe gaps")
	}
}

func TestBuildDeterministic(t *testing.T) {
	mk := func() string {
		dir := t.TempDir()
		o := Options{Dir: dir, Seed: 3, Channels: 2, Frames: 4800, TileFrames: 32,
			DamageFrame: 32, WrapAt: 4000000000, DriftPPM: 50, VFRPattern: []int{2, 2}}
		m, err := Build(o)
		if err != nil {
			t.Fatal(err)
		}
		return m.Fingerprint
	}
	if mk() != mk() {
		t.Fatal("fixture fingerprint not deterministic for identical options")
	}
}

func TestNoDamageIsClean(t *testing.T) {
	dir := t.TempDir()
	o := Options{Dir: dir, Seed: 1, Channels: 2, Frames: 4800, TileFrames: 32,
		DamageFrame: -1, WrapAt: 1000, DriftPPM: 0, VFRPattern: nil}
	m, err := Build(o)
	if err != nil {
		t.Fatal(err)
	}
	if m.DamageCount != 0 {
		t.Fatal("expected zero damage")
	}
	rd, _ := wav.NewReader(filepath.Join(dir, AudioFile))
	rep, err := rd.Inspect()
	if err != nil {
		t.Fatal(err)
	}
	if rep.Faults.Count() != 0 {
		t.Fatalf("clean fixture produced faults: %+v", rep.Faults.All())
	}
}
