package layout

import (
	"math"
	"testing"

	"avpipe/internal/media"
)

func block(channels int, samples ...float64) media.Block {
	return media.Block{
		Track: "t", Seq: 3, Start: 7, Rate: media.Rate{Num: 48000, Den: 1},
		Channels: channels, Samples: samples,
	}
}

func TestStereoToMono(t *testing.T) {
	m, err := BuildMatrix([]string{"L", "R"}, Mono)
	if err != nil {
		t.Fatal(err)
	}
	out, err := m.Apply(block(2, 1, 0, 0, 1, 1, 1))
	if err != nil {
		t.Fatal(err)
	}
	if out.Channels != 1 || out.Frames() != 3 {
		t.Fatalf("out=%+v", out)
	}
	want := []float64{0.5, 0.5, 1}
	for i, w := range want {
		if math.Abs(out.Samples[i]-w) > 1e-12 {
			t.Fatalf("sample %d=%v want %v", i, out.Samples[i], w)
		}
	}
	if out.Start != 7 || out.Seq != 3 || out.Track != "t" {
		t.Fatalf("metadata lost: %+v", out)
	}
}

func TestFiveOneToStereo(t *testing.T) {
	m, err := BuildMatrix(Surround.Labels, Stereo)
	if err != nil {
		t.Fatal(err)
	}
	// L=1 R=0 C=0 LFE=1 Ls=0 Rs=0
	out, err := m.Apply(block(6, 1, 0, 0, 1, 0, 0))
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(out.Samples[0]-1) > 1e-12 || math.Abs(out.Samples[1]) > 1e-12 {
		t.Fatalf("LFE leaked into stereo: %v", out.Samples)
	}
	// Centre channel feeds both sides at -3 dB.
	out, _ = m.Apply(block(6, 0, 0, 1, 0, 0, 0))
	const minus3 = 0.7071067811865476
	if math.Abs(out.Samples[0]-minus3) > 1e-12 || math.Abs(out.Samples[1]-minus3) > 1e-12 {
		t.Fatalf("centre downmix %v", out.Samples)
	}
	// Surround left feeds L only.
	out, _ = m.Apply(block(6, 0, 0, 0, 0, 1, 0))
	if math.Abs(out.Samples[0]-minus3) > 1e-12 || math.Abs(out.Samples[1]) > 1e-12 {
		t.Fatalf("Ls downmix %v", out.Samples)
	}
}

func TestFiveOneToMono(t *testing.T) {
	m, err := BuildMatrix(Surround.Labels, Mono)
	if err != nil {
		t.Fatal(err)
	}
	out, err := m.Apply(block(6, 1, 1, 0, 0, 0, 0))
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(out.Samples[0]-1) > 1e-12 {
		t.Fatalf("stereo sum=%v", out.Samples)
	}
	if m.Coefficient(0, 3) != 0 {
		t.Fatal("LFE must not enter mono")
	}
}

func TestMonoUpmix(t *testing.T) {
	m, err := BuildMatrix([]string{"M"}, Stereo)
	if err != nil {
		t.Fatal(err)
	}
	out, err := m.Apply(block(1, 0.25))
	if err != nil {
		t.Fatal(err)
	}
	if out.Samples[0] != 0.25 || out.Samples[1] != 0.25 {
		t.Fatalf("upmix=%v", out.Samples)
	}
}

func TestUnknownLabelsRejected(t *testing.T) {
	cases := [][]string{
		{"L", "SL"},    // unknown label
		{"L", "L"},     // duplicate
		{"", "R"},      // empty
		{"LFE", "LFE"}, // duplicates
		{"bogus"},      // unknown
	}
	for _, labels := range cases {
		if _, err := BuildMatrix(labels, Stereo); err == nil {
			t.Fatalf("labels %v accepted", labels)
		}
	}
	if _, err := BuildMatrix([]string{"L", "R"}, Layout{Name: "quad", Labels: []string{"L", "R", "Ls", "Rs"}}); err == nil {
		t.Fatal("unknown target layout accepted")
	}
	if _, err := ByName("quad"); err == nil {
		t.Fatal("ByName accepted unknown")
	}
}

func TestApplyChannelMismatch(t *testing.T) {
	m, _ := BuildMatrix(Mono.Labels, Stereo)
	if _, err := m.Apply(block(2, 1, 1)); err == nil {
		t.Fatal("mismatch accepted")
	}
	pe, ok := media.AsError(func() error { _, e := m.Apply(block(2, 1, 1)); return e }())
	if !ok || pe.Phase != media.PhaseLayout {
		t.Fatalf("bad error %v", pe)
	}
}

func TestSimultaneousChannels(t *testing.T) {
	m, _ := BuildMatrix(Surround.Labels, Mono)
	// All six channels at once; the mono gain should be the sum of the
	// non-LFE coefficients.
	out, err := m.Apply(block(6, 1, 1, 1, 1, 1, 1))
	if err != nil {
		t.Fatal(err)
	}
	want := 0.5 + 0.5 + 0.7071067811865476 + 0.5 + 0.5
	if math.Abs(out.Samples[0]-want) > 1e-12 {
		t.Fatalf("got %v want %v", out.Samples[0], want)
	}
}

func TestByNameNormalizes(t *testing.T) {
	for name, want := range map[string]string{
		"MONO": "mono", " Stereo ": "stereo", "5.1": "5.1",
	} {
		got, err := ByName(name)
		if err != nil || got.Name != want {
			t.Fatalf("ByName(%q)=%+v err=%v", name, got, err)
		}
	}
}
