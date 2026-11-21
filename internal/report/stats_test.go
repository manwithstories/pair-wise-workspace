package report

import (
	"math"
	"testing"

	"mediad/internal/clock"
	"mediad/internal/manifest"
	"mediad/internal/model"
)

func at(n, d int64) clock.Rational {
	r, err := clock.New(n, d)
	if err != nil {
		panic(err)
	}
	return r
}

func TestDeterministicPSNR(t *testing.T) {
	a := []byte{1, 2, 3, 4, 5}
	got, err := DeterministicPSNR(a, append([]byte(nil), a...))
	if err != nil {
		t.Fatal(err)
	}
	if got != 99.0 {
		t.Fatalf("identical buffers psnr=%v want 99", got)
	}
	b := []byte{1, 2, 3, 4, 6}
	got, err = DeterministicPSNR(a, b)
	if err != nil {
		t.Fatal(err)
	}
	// One of five bytes differs by one: MSE=0.2 and 10*log10(65025/0.2).
	if math.Abs(got-55.12) > 0.1 {
		t.Fatalf("psnr=%v", got)
	}
	if _, err := DeterministicPSNR(a, nil); err == nil {
		t.Fatal("empty buffer should error")
	}
}

func TestSummaryReproducible(t *testing.T) {
	tr := model.Track{ID: 1, Kind: model.Video, Timescale: 30000}
	for i := 0; i < 30; i++ {
		d := at(int64(i)*1000, 30000)
		tr.Samples = append(tr.Samples, model.Sample{Index: i, Size: 1000, DTS: d, PTS: d, Duration: at(1000, 30000)})
	}
	m, err := manifest.Build(17, "mp4", []manifest.TrackTarget{{
		TrackID: 1, End: at(1, 1), TargetBitrateKbps: 240,
	}}, []model.Track{tr})
	if err != nil {
		t.Fatal(err)
	}
	b := Builder{
		Seed: "17", TargetBitrateKbps: 240, Manifest: m,
		Quality: Quality{PSNRdB: 99, Measured: 30, Method: "deterministic bytes"},
		Loss:    LossCounts{Recovered: 2, Duplicates: 1, Reordered: 3, Unrecoverable: 0},
	}
	s1 := b.Build()
	s2 := b.Build()
	if s1.ManifestSHA256 != s2.ManifestSHA256 {
		t.Fatal("manifest digest is not reproducible")
	}
	if s1.BitrateDeltaPct != 0 || s1.ActualBitrateKbps != 240 {
		t.Fatalf("bitrate=%v delta=%v", s1.ActualBitrateKbps, s1.BitrateDeltaPct)
	}
	if s1.RecoveredPackets != 2 || s1.Duplicates != 1 {
		t.Fatalf("counters=%+v", s1)
	}
	j1, _ := s1.JSON()
	j2, _ := s2.JSON()
	if string(j1) != string(j2) {
		t.Fatal("summary JSON is not deterministic")
	}
}

func TestGate(t *testing.T) {
	if err := Gate(Summary{IndexSeekMismatches: 0, QualityPSNRdB: 40}, 30); err != nil {
		t.Fatalf("gate should pass: %v", err)
	}
	err := Gate(Summary{IndexSeekMismatches: 2, QualityPSNRdB: 40}, 30)
	if err == nil || model.KindOf(err) != model.KindThreshold {
		t.Fatalf("mismatch gate: %v", err)
	}
	err = Gate(Summary{QualityPSNRdB: 20}, 30)
	if err == nil || model.KindOf(err) != model.KindThreshold {
		t.Fatalf("quality gate: %v", err)
	}
}

func TestFaultFieldsAppear(t *testing.T) {
	s := Builder{Seed: "17", RollbackStage: "index", ResumeStage: "index", InjectedFaults: 5}.Build()
	if s.RollbackStage != "index" || s.ResumeStage != "index" || s.InjectedFaults != 5 {
		t.Fatalf("fault fields=%+v", s)
	}
}
