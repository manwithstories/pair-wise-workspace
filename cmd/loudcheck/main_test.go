package main

import (
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"testing"

	"loudcheck/internal/filter"
	"loudcheck/internal/meter"
	"loudcheck/internal/peak"
	"loudcheck/internal/report"
	"loudcheck/testdata"
)

// genFixtures builds the fixture set in a temp dir.
func genFixtures(t *testing.T, seed int64) string {
	t.Helper()
	dir := t.TempDir()
	_, err := fixture.Generate(fixture.Options{OutDir: dir, Seed: seed, Channels: 6})
	if err != nil {
		t.Fatalf("generate fixtures: %v", err)
	}
	return dir
}

func measure(t *testing.T, path string) *meter.Result {
	t.Helper()
	res, err := meter.MeasureFile(path, meter.Options{
		TargetLUFS: -23, TruePeakLimit: -1.0,
	})
	if err != nil {
		t.Fatalf("measure %s: %v", path, err)
	}
	return res
}

func TestKWeightCoefficients48k(t *testing.T) {
	// Canonical BS.1770 coefficients at 48 kHz (ITU-R BS.1770-4).
	shelfB := [3]float64{1.53512485958697, -2.69169618940638, 1.19839281085285}
	shelfA := [3]float64{1, -1.69065929318241, 0.73248077421585}
	hpB := [3]float64{1, -2, 1}
	hpA := [3]float64{1, -1.99004745483398, 0.99007225036621}

	biquad := func(b, a [3]float64, in []float64) []float64 {
		out := make([]float64, len(in))
		var x1, x2, y1, y2 float64
		for i, x := range in {
			y := b[0]*x + b[1]*x1 + b[2]*x2 - a[1]*y1 - a[2]*y2
			x2, x1 = x1, x
			y2, y1 = y1, y
			out[i] = y
		}
		return out
	}
	impulse := make([]float64, 8)
	impulse[0] = 1
	want := biquad(hpB, hpA, biquad(shelfB, shelfA, impulse))

	kw, err := filter.NewKWeight(48000)
	if err != nil {
		t.Fatal(err)
	}
	for i, x := range impulse {
		if got := kw.Process(x); math.Abs(got-want[i]) > 1e-9 {
			t.Fatalf("impulse response[%d] = %.12f, want %.12f", i, got, want[i])
		}
	}
}

func TestFixtureDeterministic(t *testing.T) {
	a := genFixtures(t, 42)
	b := genFixtures(t, 42)
	entries, err := os.ReadDir(a)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 5 {
		t.Fatalf("want 5 fixtures, got %d", len(entries))
	}
	for _, e := range entries {
		ba, _ := os.ReadFile(filepath.Join(a, e.Name()))
		bb, _ := os.ReadFile(filepath.Join(b, e.Name()))
		if len(ba) != len(bb) {
			t.Fatalf("%s: size mismatch", e.Name())
		}
		for i := range ba {
			if ba[i] != bb[i] {
				t.Fatalf("%s: byte %d differs across runs", e.Name(), i)
			}
		}
	}
}

func TestMeasureFixtures(t *testing.T) {
	dir := genFixtures(t, 42)
	cases := []struct {
		file         string
		wantLUFS     float64
		tol          float64
		wantDecision string
	}{
		{"sine_stereo_16.wav", -23.0, 0.3, meter.DecisionPass},
		{"sine_lownoise_24.wav", -28.5, 1.0, meter.DecisionPass},
		{"silence_loud_16.wav", -16.5, 0.5, meter.DecisionFail},
		{"multich_float.wav", -23.0, 0.3, meter.DecisionPass},
		{"transient_clip_16.wav", -23.0, 0.5, meter.DecisionFail},
	}
	for _, c := range cases {
		res := measure(t, filepath.Join(dir, c.file))
		if res.IntegratedLUFS == nil {
			t.Fatalf("%s: integrated undecidable", c.file)
		}
		if d := math.Abs(*res.IntegratedLUFS - c.wantLUFS); d > c.tol {
			t.Errorf("%s: integrated %.3f, want %.3f±%.1f", c.file, *res.IntegratedLUFS, c.wantLUFS, c.tol)
		}
		if res.Decision != c.wantDecision {
			t.Errorf("%s: decision %s, want %s", c.file, res.Decision, c.wantDecision)
		}
		if res.Windows == 0 || res.GatedWindows == 0 {
			t.Errorf("%s: no windows counted", c.file)
		}
	}
	// Clipped file must show a hot true peak; clean sine must not.
	clip := measure(t, filepath.Join(dir, "transient_clip_16.wav"))
	if clip.TruePeakDBTP == nil || *clip.TruePeakDBTP < -0.2 {
		t.Errorf("clip: true peak %v, want > -0.2 dBTP", clip.TruePeakDBTP)
	}
	clean := measure(t, filepath.Join(dir, "sine_stereo_16.wav"))
	if clean.TruePeakDBTP == nil || *clean.TruePeakDBTP > -20 {
		t.Errorf("clean: true peak %v, want < -20 dBTP", clean.TruePeakDBTP)
	}
	if clean.LFEIncluded {
		t.Errorf("stereo file must not report LFE")
	}
	multi := measure(t, filepath.Join(dir, "multich_float.wav"))
	if !multi.LFEIncluded {
		t.Errorf("5.1 file must report LFE presence")
	}
}

// writeSilenceWAV writes a minimal 16-bit stereo silence file.
func writeSilenceWAV(t *testing.T, path string, frames int) {
	t.Helper()
	data := make([]byte, frames*4)
	hdr := make([]byte, 44)
	copy(hdr[0:], "RIFF")
	binary.LittleEndian.PutUint32(hdr[4:], uint32(36+len(data)))
	copy(hdr[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(hdr[16:], 16)
	binary.LittleEndian.PutUint16(hdr[20:], 1)
	binary.LittleEndian.PutUint16(hdr[22:], 2)
	binary.LittleEndian.PutUint32(hdr[24:], 48000)
	binary.LittleEndian.PutUint32(hdr[28:], 48000*4)
	binary.LittleEndian.PutUint16(hdr[32:], 4)
	binary.LittleEndian.PutUint16(hdr[34:], 16)
	copy(hdr[36:], "data")
	binary.LittleEndian.PutUint32(hdr[40:], uint32(len(data)))
	if err := os.WriteFile(path, append(hdr, data...), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestSilenceUndecidable(t *testing.T) {
	p := filepath.Join(t.TempDir(), "silence.wav")
	writeSilenceWAV(t, p, 48000)
	res := measure(t, p)
	if res.Decision != meter.DecisionUndecidable {
		t.Fatalf("silence: decision %s, want undecidable", res.Decision)
	}
	if res.IntegratedLUFS != nil {
		t.Errorf("silence: integrated must be null, got %v", *res.IntegratedLUFS)
	}
}

func TestTruePeakOvershoot(t *testing.T) {
	// A half-scale sine must read ≈ -6.02 dBTP after 4x oversampling.
	tp, err := peak.NewTruePeak(4)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4800; i++ {
		tp.Process(0.5 * math.Sin(2*math.Pi*997*float64(i)/48000))
	}
	tp.Finish()
	db, ok := peak.DBTP(tp.Max())
	if !ok || math.Abs(db+6.02) > 0.3 {
		t.Errorf("sine true peak %.3f, want ≈ -6.02", db)
	}
}

func TestFixed3JSON(t *testing.T) {
	b, err := report.Fixed3(-23).MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "-23.000" {
		t.Errorf("Fixed3 marshal = %s, want -23.000", b)
	}
}

// runCLI executes the CLI with the given args and returns the exit code.
func runCLI(args ...string) int {
	old := os.Args
	os.Args = append([]string{"loudcheck"}, args...)
	defer func() { os.Args = old }()
	return run()
}

func TestExitCodes(t *testing.T) {
	dir := genFixtures(t, 42)

	if code := runCLI("batch", "--input", dir); code != 1 {
		t.Errorf("mixed batch: exit %d, want 1", code)
	}
	if code := runCLI("batch"); code != 2 {
		t.Errorf("missing input: exit %d, want 2", code)
	}
	if code := runCLI("batch", "--input", dir, "--peak-factor", "3"); code != 2 {
		t.Errorf("bad peak factor: exit %d, want 2", code)
	}
	if code := runCLI("measure", "--input", filepath.Join(dir, "sine_stereo_16.wav")); code != 0 {
		t.Errorf("passing measure: exit %d, want 0", code)
	}
	if code := runCLI("measure", "--input", filepath.Join(dir, "silence_loud_16.wav")); code != 1 {
		t.Errorf("failing measure: exit %d, want 1", code)
	}

	// A corrupt file inside the batch must surface as exit 3.
	bad := filepath.Join(dir, "corrupt.wav")
	if err := os.WriteFile(bad, []byte("not a wav file at all"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code := runCLI("batch", "--input", dir); code != 3 {
		t.Errorf("corrupt batch: exit %d, want 3", code)
	}
	os.Remove(bad)
}

func TestWAVRejects(t *testing.T) {
	dir := t.TempDir()
	mk := func(name string, mutate func(hdr []byte)) string {
		hdr := make([]byte, 44)
		copy(hdr[0:], "RIFF")
		binary.LittleEndian.PutUint32(hdr[4:], 36)
		copy(hdr[8:], "WAVEfmt ")
		binary.LittleEndian.PutUint32(hdr[16:], 16)
		binary.LittleEndian.PutUint16(hdr[20:], 1)
		binary.LittleEndian.PutUint16(hdr[22:], 2)
		binary.LittleEndian.PutUint32(hdr[24:], 48000)
		binary.LittleEndian.PutUint32(hdr[28:], 48000*4)
		binary.LittleEndian.PutUint16(hdr[32:], 4)
		binary.LittleEndian.PutUint16(hdr[34:], 16)
		copy(hdr[36:], "data")
		if mutate != nil {
			mutate(hdr)
		}
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, hdr, 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	rejects := map[string]func([]byte){
		"compressed.wav": func(h []byte) { binary.LittleEndian.PutUint16(h[20:], 0x55) }, // MP3
		"nine_ch.wav":    func(h []byte) { binary.LittleEndian.PutUint16(h[22:], 9) },
		"odd_data.wav":   func(h []byte) { binary.LittleEndian.PutUint32(h[40:], 7) },
		"bad_rate.wav":   func(h []byte) { binary.LittleEndian.PutUint32(h[24:], 100) },
	}
	for name, mutate := range rejects {
		if _, err := meter.MeasureFile(mk(name, mutate), meter.Options{}); err == nil {
			t.Errorf("%s: expected rejection", name)
		}
	}
}
