package manifest

import (
	"testing"

	"mediad/internal/clock"
	"mediad/internal/model"
)

func at(n, d int64) clock.Rational {
	r, err := clock.New(n, d)
	if err != nil {
		panic(err)
	}
	return r
}

// track builds a track whose samples are 1/30 s apart.
func track(id uint32, count int, bytesPerSample uint32) model.Track {
	tr := model.Track{ID: id, Kind: model.Video, Timescale: 30000, Codec: "avc1"}
	for i := 0; i < count; i++ {
		d := at(int64(i)*1000, 30000)
		tr.Samples = append(tr.Samples, model.Sample{
			Index: i, Size: bytesPerSample, DTS: d, PTS: d, Duration: at(1000, 30000),
		})
	}
	return tr
}

func TestBuildExactAlignment(t *testing.T) {
	tr := track(1, 30, 1000) // 1 second of 1000-byte samples = 240 kbps
	targets := []TrackTarget{{
		TrackID: 1, Start: at(0, 1), End: at(1, 1), TargetBitrateKbps: 240,
	}}
	m, err := Build(17, "mp4", targets, []model.Track{tr})
	if err != nil {
		t.Fatal(err)
	}
	if m.AlignmentUS != 0 {
		t.Fatalf("alignment=%d us", m.AlignmentUS)
	}
	if len(m.Tracks) != 1 {
		t.Fatalf("tracks=%d", len(m.Tracks))
	}
	r := m.Tracks[0]
	if r.ActualBitrateKbps != 240 {
		t.Fatalf("actual bitrate=%.3f want 240", r.ActualBitrateKbps)
	}
	if r.BitrateDeltaPct != 0 {
		t.Fatalf("delta=%.3f", r.BitrateDeltaPct)
	}
}

func TestBuildReportsMisalignment(t *testing.T) {
	tr := track(1, 30, 1000)
	// Target end is one microsecond later than the media.
	targets := []TrackTarget{{
		TrackID: 1, Start: at(0, 1), End: at(1_000_001, 1_000_000), TargetBitrateKbps: 240,
	}}
	m, err := Build(17, "mp4", targets, []model.Track{tr})
	if err != nil {
		t.Fatal(err)
	}
	if m.Tracks[0].EndErrorUS != -1 {
		t.Fatalf("end error=%d us want -1", m.Tracks[0].EndErrorUS)
	}
	if m.AlignedMS() != 0.001 {
		t.Fatalf("align ms=%v", m.AlignedMS())
	}
	if err := m.Gate(0.001, 5); err != nil {
		t.Fatalf("gate should pass at the boundary: %v", err)
	}
	if err := m.Gate(0.0005, 5); err == nil {
		t.Fatal("gate should fail below the observed error")
	}
}

func TestBuildRejectsUnknownTrack(t *testing.T) {
	tr := track(2, 10, 100)
	targets := []TrackTarget{{TrackID: 1, End: at(1, 1), TargetBitrateKbps: 8}}
	if _, err := Build(1, "mp4", targets, []model.Track{tr}); err == nil {
		t.Fatal("media track missing from manifest should fail")
	}
	if _, err := Build(1, "mp4", []TrackTarget{{TrackID: 9, End: at(1, 1)}}, []model.Track{tr}); err == nil {
		t.Fatal("manifest track missing from media should fail")
	}
}

func TestDigestIsStableAndOrderIndependent(t *testing.T) {
	a := track(1, 30, 1000)
	b := track(2, 30, 500)
	targets := []TrackTarget{
		{TrackID: 2, End: at(1, 1), TargetBitrateKbps: 4},
		{TrackID: 1, End: at(1, 1), TargetBitrateKbps: 240},
	}
	m1, err := Build(17, "mp4", targets, []model.Track{a, b})
	if err != nil {
		t.Fatal(err)
	}
	m2, err := Build(17, "mp4", targets, []model.Track{b, a})
	if err != nil {
		t.Fatal(err)
	}
	d1, _ := m1.Digest()
	d2, _ := m2.Digest()
	if d1 != d2 {
		t.Fatalf("digest depends on input order: %s vs %s", d1, d2)
	}
	if len(d1) != 64 {
		t.Fatalf("digest length %d", len(d1))
	}
}

func TestGateBitrate(t *testing.T) {
	tr := track(1, 30, 1050) // 252 kbps, 5% over the 240 kbps target
	targets := []TrackTarget{{TrackID: 1, End: at(1, 1), TargetBitrateKbps: 240}}
	m, err := Build(1, "mp4", targets, []model.Track{tr})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Gate(1, 10); err != nil {
		t.Fatalf("10%% gate should pass at 5%%: %v", err)
	}
	err = m.Gate(1, 4)
	if err == nil {
		t.Fatal("4% gate should fail at 5%")
	}
	if model.KindOf(err) != model.KindThreshold {
		t.Fatalf("kind=%v", model.KindOf(err))
	}
}

func TestQualityCountersPassThrough(t *testing.T) {
	tr := track(1, 3, 100)
	tr.Samples[1].Damaged = true
	tr.Samples[2].Synthesised = true
	targets := []TrackTarget{{TrackID: 1, End: at(3, 30), TargetBitrateKbps: 8}}
	m, err := Build(1, "mp4", targets, []model.Track{tr})
	if err != nil {
		t.Fatal(err)
	}
	r := m.Tracks[0]
	if r.DamagedSamples != 1 || r.SynthesisedSamples != 1 {
		t.Fatalf("counters=%+v", r)
	}
}
