package audio

import (
	"math"
	"testing"
)

// phase gives a smooth deterministic angle for synthetic test tones.
func phase(i int) float64 {
	return math.Pi * float64(i) / 32
}

func TestConvertSampleRateAndChannels(t *testing.T) {
	// 24 kHz mono, 480 frames -> 48 kHz stereo, 960 frames.
	src := make([]int16, 480)
	for i := range src {
		src[i] = int16(1000 * math.Sin(float64(i)/8))
	}
	out, br, err := Convert(Frame{SampleRate: 24000, Channels: 1, Samples: src})
	if err != nil {
		t.Fatal(err)
	}
	if br.Preserved {
		t.Fatal("long enough fragment must convert")
	}
	if out.SampleRate != TargetRate || out.Channels != TargetChannels {
		t.Fatalf("target format: %+v", out)
	}
	if want := 960 * TargetChannels; len(out.Samples) != want {
		t.Fatalf("out samples: got %d want %d", len(out.Samples), want)
	}
	// Mono dual-mix: both channels identical per frame.
	if out.Samples[0] != out.Samples[1] {
		t.Fatal("mono source must map to dual mono")
	}
}

func TestBoundaryPreserved(t *testing.T) {
	src := []int16{120, -120, 33} // 3 mono frames, below filter tail
	out, br, err := Convert(Frame{SampleRate: 16000, Channels: 1, Samples: src})
	if err != nil {
		t.Fatal(err)
	}
	if !br.Preserved || br.Reason == "" {
		t.Fatalf("short boundary: %+v", br)
	}
	if len(out.Samples) != len(src) {
		t.Fatal("preserved fragment must remain byte-for-byte framed")
	}
}

func TestMalformedFrame(t *testing.T) {
	if _, _, err := Convert(Frame{SampleRate: 48000, Channels: 2, Samples: []int16{1, 2, 3}}); err == nil {
		t.Fatal("odd sample count for stereo must error")
	}
}

func TestNormalizeLoudness(t *testing.T) {
	// One full 400 ms block of moderate-level stereo tone.
	n := blockFrames
	samples := make([]int16, n*2)
	for i := 0; i < n; i++ {
		v := int16(3000 * math.Sin(2*phase(i)))
		samples[i*2], samples[i*2+1] = v, v
	}
	out, stats := Normalize(Frame{SampleRate: TargetRate, Channels: 2, Samples: samples})
	if stats.GatedBlocks != 1 {
		t.Fatalf("gated blocks: %d", stats.GatedBlocks)
	}
	if math.IsInf(stats.IntegratedLUFS, 0) || stats.GainDB == 0 {
		t.Fatalf("tonal block must measure and gain: %+v", stats)
	}
	if len(out.Samples) != len(samples) {
		t.Fatal("normalize must preserve sample count")
	}
}

func TestSilenceAndClip(t *testing.T) {
	n := blockFrames
	samples := make([]int16, n*2) // digital silence
	_, stats := Normalize(Frame{SampleRate: TargetRate, Channels: 2, Samples: samples})
	if stats.SilenceFrames != n {
		t.Fatalf("silence frames: got %d want %d", stats.SilenceFrames, n)
	}
	if stats.GainDB != 0 || stats.ClipFrames != 0 {
		t.Fatalf("silent fragment must not gain or clip: %+v", stats)
	}
	// ApplyGain is the shared limiting path: an over-driven gain must clamp
	// to full scale and report the affected windows instead of wrapping.
	const clipWin = 480 // 10 ms stereo window used by ApplyGain
	src := make([]int16, clipWin*2)
	src[0], src[1] = 30000, -30000
	gained, clipFrames := ApplyGain(src, 2.0)
	if clipFrames != clipWin {
		t.Fatalf("clip frames: got %d want %d", clipFrames, clipWin)
	}
	if gained[0] != 32767 || gained[1] != -32768 {
		t.Fatalf("clamp: got %d/%d", gained[0], gained[1])
	}
	// Determinism: the same input and gain yields identical samples.
	again, _ := ApplyGain(src, 2.0)
	for i := range gained {
		if gained[i] != again[i] {
			t.Fatal("ApplyGain is not deterministic")
		}
	}
	// Normalize itself stays deterministic on a tonal fragment.
	loud := make([]int16, n*2)
	for i := 0; i < n; i++ {
		v := int16(500 * math.Sin(2*phase(i)))
		loud[i*2], loud[i*2+1] = v, v
	}
	out2, st2 := Normalize(Frame{SampleRate: TargetRate, Channels: 2, Samples: loud})
	out3, _ := Normalize(Frame{SampleRate: TargetRate, Channels: 2, Samples: loud})
	for i := range out2.Samples {
		if out2.Samples[i] != out3.Samples[i] {
			t.Fatal("normalize is not deterministic")
		}
	}
	if len(out2.Samples) != len(loud) {
		t.Fatalf("frame count changed: %d -> %d", len(loud), len(out2.Samples))
	}
	if st2.ClipFrames != 0 {
		t.Fatalf("a -23 LUFS tonal anchor must not clip: %+v", st2)
	}
}
