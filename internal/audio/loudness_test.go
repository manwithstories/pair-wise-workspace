package audio

import (
	"encoding/binary"
	"math"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
)

// TestLoudnessMatchesFFmpeg cross-checks the BS.1770 implementation against
// the ebur128 reference in FFmpeg. It is skipped when ffmpeg is unavailable.
func TestLoudnessMatchesFFmpeg(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not available")
	}
	const seconds = 10
	in := sineBuffer(48000, 48000*seconds, 0.1, 440, 2)
	m := NewMeter(2)
	if err := m.Add(in); err != nil {
		t.Fatal(err)
	}
	got := m.Result()
	if !got.Valid {
		t.Fatal("measurement invalid")
	}
	want := ffmpegLoudness(t, in)
	if math.Abs(got.IntegratedLUFS-want) > 0.5 {
		t.Fatalf("integrated %.2f LUFS, ffmpeg says %.2f", got.IntegratedLUFS, want)
	}
}

// TestLoudnessTargetAchievable verifies gain normalization lands on -23 LUFS
// and respects the peak ceiling.
func TestLoudnessTargetAchievable(t *testing.T) {
	in := sineBuffer(48000, 48000*5, 0.4, 1000, 2)
	m := NewMeter(2)
	if err := m.Add(in); err != nil {
		t.Fatal(err)
	}
	l := m.Result()
	gain, limited := GainForTarget(l, TargetLUFS)
	if limited {
		t.Fatalf("unexpected peak limiting at gain %.3f", gain)
	}
	out := ApplyGain(in, gain)
	m2 := NewMeter(2)
	if err := m2.Add(out); err != nil {
		t.Fatal(err)
	}
	got := m2.Result()
	if math.Abs(got.IntegratedLUFS-TargetLUFS) > 0.1 {
		t.Fatalf("after gain: %.2f LUFS (target %.1f)", got.IntegratedLUFS, TargetLUFS)
	}
	if got.SamplePeak > TruePeakCeiling+1e-6 {
		t.Fatalf("peak %.3f exceeds ceiling", got.SamplePeak)
	}
}

func TestLoudnessSilence(t *testing.T) {
	in := Buffer{Rate: 48000, Planes: [][]float32{make([]float32, 48000), make([]float32, 48000)}}
	m := NewMeter(2)
	if err := m.Add(in); err != nil {
		t.Fatal(err)
	}
	if l := m.Result(); l.Valid {
		t.Fatalf("silence measured as valid: %+v", l)
	}
}

func TestLoudnessRejectsWrongRate(t *testing.T) {
	m := NewMeter(2)
	if err := m.Add(sineBuffer(44100, 4410, 0.1, 440, 2)); err == nil {
		t.Fatal("44100 Hz accepted by meter")
	}
}

func ffmpegLoudness(t *testing.T, b Buffer) float64 {
	t.Helper()
	path := t.TempDir() + "/probe.wav"
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	n := b.Frames()
	data := make([]byte, n*b.Channels()*4)
	for i := 0; i < n; i++ {
		for c := 0; c < b.Channels(); c++ {
			binary.LittleEndian.PutUint32(data[(i*b.Channels()+c)*4:], math.Float32bits(b.Planes[c][i]))
		}
	}
	w := func(v any) { binary.Write(f, binary.LittleEndian, v) }
	f.WriteString("RIFF")
	w(uint32(36 + len(data)))
	f.WriteString("WAVEfmt ")
	w(uint32(16))
	w(uint16(3))
	w(uint16(b.Channels()))
	w(uint32(b.Rate))
	w(uint32(b.Rate * b.Channels() * 4))
	w(uint16(b.Channels() * 4))
	w(uint16(32))
	f.WriteString("data")
	w(uint32(len(data)))
	f.Write(data)
	f.Close()

	out, err := exec.Command("ffmpeg", "-hide_banner", "-nostats", "-i", path,
		"-af", "ebur128=framelog=quiet", "-f", "null", "-").CombinedOutput()
	if err != nil {
		t.Fatalf("ffmpeg: %v\n%s", err, out)
	}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "I:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				v, err := strconv.ParseFloat(fields[1], 64)
				if err == nil {
					return v
				}
			}
		}
	}
	t.Fatalf("no integrated loudness in ffmpeg output:\n%s", out)
	return 0
}

func TestMeterBlockGate(t *testing.T) {
	// 1 s of level then 9 s of near silence: the absolute gate must drop the
	// quiet blocks so the result reflects the loud section alone.
	loud := sineBuffer(48000, 48000, 0.2, 440, 2)
	quiet := sineBuffer(48000, 48000*9, 0.0001, 440, 2)
	m := NewMeter(2)
	if err := m.Add(loud); err != nil {
		t.Fatal(err)
	}
	if err := m.Add(quiet); err != nil {
		t.Fatal(err)
	}
	l := m.Result()
	if !l.Valid {
		t.Fatal("measurement invalid")
	}
	// Compare against the loud excerpt measured on its own.
	lm := NewMeter(2)
	if err := lm.Add(loud); err != nil {
		t.Fatal(err)
	}
	only := lm.Result()
	if math.Abs(l.IntegratedLUFS-only.IntegratedLUFS) > 1.0 {
		t.Fatalf("silence dragged loudness to %.2f (loud alone %.2f)", l.IntegratedLUFS, only.IntegratedLUFS)
	}
	if l.GatedBlocks >= l.TotalBlocks {
		t.Fatalf("quiet blocks were not gated: %d/%d", l.GatedBlocks, l.TotalBlocks)
	}
}

func TestGainPeakLimiting(t *testing.T) {
	l := Loudness{Valid: true, IntegratedLUFS: -40, SamplePeak: 0.9}
	g, limited := GainForTarget(l, TargetLUFS)
	if !limited {
		t.Fatal("expected peak limiting")
	}
	if l.SamplePeak*g > TruePeakCeiling+1e-9 {
		t.Fatalf("gain %.3f pushes peak to %.3f", g, l.SamplePeak*g)
	}
}

func TestResamplerFlushTail(t *testing.T) {
	r := NewResampler(44100, 48000)
	in := sineBuffer(44100, 4410, 0.5, 440, 2)
	out, err := r.Process(in)
	if err != nil {
		t.Fatal(err)
	}
	tail := r.Flush()
	if tail.Frames() == 0 {
		t.Fatal("flush produced no tail")
	}
	if out.Frames()+tail.Frames() < 4000 {
		t.Fatalf("total output too short: %d+%d", out.Frames(), tail.Frames())
	}
}

func TestDownmix5Point1(t *testing.T) {
	n, err := NewNormalizer(48000, 6, "5.1")
	if err != nil {
		t.Fatal(err)
	}
	in := Buffer{Rate: 48000, Planes: make([][]float32, 6)}
	for c := range in.Planes {
		in.Planes[c] = make([]float32, 4800)
		for i := range in.Planes[c] {
			in.Planes[c][i] = 0.1
		}
	}
	out, err := n.Process(in)
	if err != nil {
		t.Fatal(err)
	}
	if out.Channels() != 2 {
		t.Fatalf("channels %d", out.Channels())
	}
	if p := Peak(out); p > 1 {
		t.Fatalf("downmix clips at %.3f", p)
	}
}

func TestDeinterleaveS16(t *testing.T) {
	buf := make([]byte, 2*2*2)
	put := func(off int, v int16) { buf[off] = byte(v); buf[off+1] = byte(uint16(v) >> 8) }
	put(0, 16384)
	put(2, -16384)
	put(4, 32767)
	put(6, -32768)
	b, err := Deinterleave(buf, 2, 2, "s16")
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(float64(b.Planes[0][0])-0.5) > 1e-6 || math.Abs(float64(b.Planes[1][1])+1) > 1e-6 {
		t.Fatalf("s16: %+v", b.Planes)
	}
}
