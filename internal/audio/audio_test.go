package audio

import (
	"encoding/binary"
	"math"
	"testing"

	"github.com/asticode/go-astiav"
)

// makeL16 生成指定规格的正弦波 L16 负载；多声道使用不同幅度，避免下混后静音。
func makeL16(rate, chans int, cycles float64, peak float64) []byte {
	samples := rate / 10 // 100ms
	out := make([]byte, samples*chans*2)
	for i := 0; i < samples; i++ {
		base := peak * math.Sin(2*math.Pi*cycles*float64(i)/float64(samples))
		for c := 0; c < chans; c++ {
			amp := base * (1.0 - 0.25*float64(c)) // 各声道幅度递减但非零
			s := int16(amp * math.MaxInt16)
			binary.LittleEndian.PutUint16(out[(i*chans+c)*2:], uint16(s))
		}
	}
	return out
}

func TestDecodeRejectsUnsupportedAndBadPayload(t *testing.T) {
	if _, err := NewDecoder(44100, 2); !IsUnsupported(err) {
		t.Fatalf("44.1k must be unsupported, got %v", err)
	}
	if _, err := NewDecoder(48000, 3); !IsUnsupported(err) {
		t.Fatalf("3 channels must be unsupported, got %v", err)
	}
	d, err := NewDecoder(48000, 2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Decode([]byte{1, 2, 3}); !IsBadPayload(err) {
		t.Fatalf("odd payload must be bad: %v", err)
	}
	if _, err := d.Decode(nil); !IsBadPayload(err) {
		t.Fatalf("nil payload must be bad: %v", err)
	}
}

func TestAllInputSpecsNormalizeTo48kStereo(t *testing.T) {
	specs := []struct {
		rate, chans int
	}{
		{8000, 1}, {16000, 2}, {48000, 2}, {96000, 6},
	}
	for _, sp := range specs {
		t.Run("", func(t *testing.T) {
			dec, err := NewDecoder(sp.rate, sp.chans)
			if err != nil {
				t.Fatal(err)
			}
			norm, err := NewNormalizer(sp.rate, sp.chans)
			if err != nil {
				t.Fatal(err)
			}
			defer norm.Free()
			payload := makeL16(sp.rate, sp.chans, 4, 0.5)
			src, err := dec.Decode(payload)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			dst, err := norm.Process(src)
			src.Free()
			if err != nil {
				t.Fatalf("normalize: %v", err)
			}
			defer dst.Free()
			if dst.SampleRate() != TargetRate || dst.ChannelLayout().Channels() != TargetChans {
				t.Fatalf("output %dHz %dch", dst.SampleRate(), dst.ChannelLayout().Channels())
			}
			if dst.SampleFormat() != astiav.SampleFormatFltp {
				t.Fatalf("format=%v", dst.SampleFormat())
			}
			// 输出样本数应与输入时长匹配（100ms → 约 4800 样本，容差含重采样滤波器延迟）。
			wantSamples := sp.rate / 10 * TargetRate / sp.rate
			got := dst.NbSamples()
			if math.Abs(float64(got-wantSamples)) > 128 {
				t.Fatalf("samples=%d want ~%d", got, wantSamples)
			}
			// 两声道都必须有非零能量（下混不能抹掉信号）。
			for ch := 0; ch < TargetChans; ch++ {
				if energy(planeBytes(dst, ch)) == 0 {
					t.Fatalf("channel %d silent after downmix", ch)
				}
			}
		})
	}
}

func TestLoudnessHitsTargetAndTruePeak(t *testing.T) {
	dec, _ := NewDecoder(48000, 2)
	norm, _ := NewNormalizer(48000, 2)
	defer norm.Free()
	// 偏低响度的信号（峰值 0.1），需要较大增益才能到 -16 LUFS。
	payload := makeL16(48000, 2, 8, 0.1)
	src, err := dec.Decode(payload)
	if err != nil {
		t.Fatal(err)
	}
	dst, err := norm.Process(src)
	src.Free()
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Free()

	meter := NewLoudnessMeter()
	meter.Observe(dst)
	before := meter.LUFS()
	gain := meter.GainDB()
	if gain <= 0 {
		t.Fatalf("quiet signal needs positive gain, got %f dB", gain)
	}
	clips := ApplyGain(dst, gain)
	after := NewLoudnessMeter()
	after.Observe(dst)
	// 施加增益后响度应朝目标移动并接近 -16 LUFS（正弦测试信号与节目响度有偏差，放宽到 2）。
	if math.Abs(after.LUFS()-TargetLUFS) > 2.0 {
		t.Fatalf("loudness before=%f after=%f gain=%f", before, after.LUFS(), gain)
	}
	// 真峰值不得超过 -1dBTP（严格判定）。
	for ch := 0; ch < TargetChans; ch++ {
		for _, s := range bytesToF32(planeBytes(dst, ch)) {
			if math.Abs(float64(s)) > TruePeakLimitLinear {
				t.Fatalf("true peak violated: %f", s)
			}
		}
	}
	_ = clips
}

func TestTruePeakLimiterOnHotSignal(t *testing.T) {
	// 满幅信号 + 增益必然超限，软限制必须把所有样本压回上限内。
	dec, _ := NewDecoder(48000, 2)
	src, _ := dec.Decode(makeL16(48000, 2, 2, 1.0))
	norm, _ := NewNormalizer(48000, 2)
	defer norm.Free()
	dst, _ := norm.Process(src)
	src.Free()
	defer dst.Free()
	clips := ApplyGain(dst, 6.0) // +6dB 必然削顶
	if clips == 0 {
		t.Fatal("hot signal must trigger limiter")
	}
	for ch := 0; ch < TargetChans; ch++ {
		for _, s := range bytesToF32(planeBytes(dst, ch)) {
			if math.Abs(float64(s)) > TruePeakLimitLinear {
				t.Fatalf("limiter failed: %f", s)
			}
		}
	}
}

func TestS16PackingRoundTrip(t *testing.T) {
	dec, _ := NewDecoder(48000, 2)
	src, _ := dec.Decode(makeL16(48000, 2, 3, 0.5))
	norm, _ := NewNormalizer(48000, 2)
	defer norm.Free()
	dst, _ := norm.Process(src)
	src.Free()
	defer dst.Free()
	pcm, err := FrameToInterleavedS16(dst)
	if err != nil {
		t.Fatal(err)
	}
	// 48k 立体声 16bit：每样本 4 字节。
	if len(pcm) != dst.NbSamples()*TargetChans*2 {
		t.Fatalf("pcm bytes=%d want %d", len(pcm), dst.NbSamples()*TargetChans*2)
	}
}

func energy(b []byte) float64 {
	var sum float64
	for _, v := range bytesToF32(b) {
		sum += float64(v) * float64(v)
	}
	return sum
}
