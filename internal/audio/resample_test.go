package audio

import (
	"math"
	"testing"
)

func sineBuffer(rate, frames int, amplitude, hz float64, channels int) Buffer {
	b := Buffer{Rate: rate, Planes: make([][]float32, channels)}
	for c := range b.Planes {
		b.Planes[c] = make([]float32, frames)
		for i := range b.Planes[c] {
			t := float64(i) / float64(rate)
			b.Planes[c][i] = float32(amplitude * math.Sin(2*math.Pi*hz*t))
		}
	}
	return b
}

func TestDownmixMatrix(t *testing.T) {
	m, err := DownmixMatrix(1, "mono")
	if err != nil || m[0] != [2]float64{1, 1} {
		t.Fatalf("mono: %v %v", m, err)
	}
	m, err = DownmixMatrix(2, "stereo")
	if err != nil || m[0] != [2]float64{1, 0} || m[1] != [2]float64{0, 1} {
		t.Fatalf("stereo: %v %v", m, err)
	}
	m, err = DownmixMatrix(6, "5.1")
	if err != nil {
		t.Fatal(err)
	}
	if m[3][0] != 0 || m[3][1] != 0 {
		t.Fatalf("LFE should be excluded: %v", m[3])
	}
	var maxL, maxR float64
	for _, g := range m {
		maxL += math.Abs(g[0])
		maxR += math.Abs(g[1])
	}
	if maxL > 1.0001 || maxR > 1.0001 {
		t.Fatalf("matrix can clip: %f %f", maxL, maxR)
	}
}

func TestDeinterleave(t *testing.T) {
	// fltp: two planes of 2 frames
	buf := make([]byte, 2*2*4)
	put := func(off int, f float32) {
		u := math.Float32bits(f)
		buf[off] = byte(u)
		buf[off+1] = byte(u >> 8)
		buf[off+2] = byte(u >> 16)
		buf[off+3] = byte(u >> 24)
	}
	put(0, 0.25)
	put(4, -0.5)
	put(8, 0.75)
	put(12, 1.0)
	b, err := Deinterleave(buf, 2, 2, "fltp")
	if err != nil {
		t.Fatal(err)
	}
	if b.Planes[0][0] != 0.25 || b.Planes[0][1] != -0.5 || b.Planes[1][0] != 0.75 || b.Planes[1][1] != 1 {
		t.Fatalf("fltp: %+v", b.Planes)
	}
	if _, err := Deinterleave(buf[:8], 2, 2, "fltp"); err == nil {
		t.Fatal("short buffer accepted")
	}
	if _, err := Deinterleave(buf, 2, 2, "nope"); err == nil {
		t.Fatal("unknown format accepted")
	}
}

func TestResamplerPassThrough(t *testing.T) {
	r := NewResampler(48000, 48000)
	in := sineBuffer(48000, 100, 0.5, 440, 2)
	out, err := r.Process(in)
	if err != nil || out.Frames() != in.Frames() {
		t.Fatalf("passthrough: %d %v", out.Frames(), err)
	}
}

func TestResamplerLengthAndRate(t *testing.T) {
	cases := []struct{ src, dst int }{
		{44100, 48000}, {48000, 44100}, {22050, 48000}, {96000, 48000},
	}
	for _, c := range cases {
		r := NewResampler(c.src, c.dst)
		in := sineBuffer(c.src, c.src, 0.5, 440, 2)
		out, err := r.Process(in)
		if err != nil {
			t.Fatalf("%d->%d: %v", c.src, c.dst, err)
		}
		if out.Rate != c.dst {
			t.Fatalf("rate %d", out.Rate)
		}
		want := float64(c.dst)
		got := float64(out.Frames())
		if math.Abs(got-want) > 0.05*want {
			t.Fatalf("%d->%d produced %d frames, want about %.0f", c.src, c.dst, out.Frames(), want)
		}
		if p := Peak(out); p == 0 || p > 1 {
			t.Fatalf("%d->%d peak %f", c.src, c.dst, p)
		}
		// Flush must add tail without exploding.
		tail := r.Flush()
		if tail.Rate != c.dst {
			t.Fatalf("flush rate %d", tail.Rate)
		}
	}
}

func TestResamplerPreservesSineFrequency(t *testing.T) {
	// 440 Hz at 48k -> 44.1k -> 48k; measure zero crossings stay near 440 Hz.
	r1 := NewResampler(48000, 44100)
	in := sineBuffer(48000, 48000, 0.5, 440, 1)
	mid, err := r1.Process(in)
	if err != nil {
		t.Fatal(err)
	}
	r2 := NewResampler(44100, 48000)
	out, err := r2.Process(mid)
	if err != nil {
		t.Fatal(err)
	}
	sig := out.Planes[0]
	crossings := 0
	skip := 1000
	for i := skip + 1; i < len(sig)-skip; i++ {
		if (sig[i-1] < 0 && sig[i] >= 0) || (sig[i-1] >= 0 && sig[i] < 0) {
			crossings++
		}
	}
	seconds := float64(len(sig)-2*skip) / 48000
	hz := float64(crossings) / 2 / seconds
	if math.Abs(hz-440) > 5 {
		t.Fatalf("frequency drifted to %.1f Hz", hz)
	}
}

func TestNormalizerMonoToStereo(t *testing.T) {
	n, err := NewNormalizer(44100, 1, "mono")
	if err != nil {
		t.Fatal(err)
	}
	in := sineBuffer(44100, 44100, 0.5, 440, 1)
	out, err := n.Process(in)
	if err != nil {
		t.Fatal(err)
	}
	if out.Rate != TargetRate || out.Channels() != 2 {
		t.Fatalf("normalized to %d Hz %d ch", out.Rate, out.Channels())
	}
	if out.Planes[0][100] != out.Planes[1][100] {
		t.Fatal("mono should duplicate to both channels")
	}
}

func TestNormalizerRejectsChannelChange(t *testing.T) {
	n, _ := NewNormalizer(48000, 2, "stereo")
	if _, err := n.Process(sineBuffer(48000, 100, 0.1, 100, 1)); err == nil {
		t.Fatal("channel change accepted")
	}
}
