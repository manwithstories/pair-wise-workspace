package audio

import (
	"math"
	"testing"

	"archivemedia/internal/packet"
)

func toneFrame(rate, channels, nb int, freq float64) *packet.PCMFrame {
	s := make([]float32, nb*channels)
	step := 2 * math.Pi * freq / float64(rate)
	for i := 0; i < nb; i++ {
		v := float32(0.25 * math.Sin(step*float64(i)))
		for c := 0; c < channels; c++ {
			s[i*channels+c] = v
		}
	}
	return &packet.PCMFrame{Samples: s, Channels: channels, SampleRate: rate, NbSamples: nb}
}

func TestResampleMonoToStereo48k(t *testing.T) {
	r := NewResampler()
	defer r.Close()
	// 24kHz 单声道 → 48kHz 立体声。
	in := toneFrame(24000, 1, 2400, 440)
	out, err := r.Convert(in)
	if err != nil {
		t.Fatal(err)
	}
	if out.Frames < 4700 || out.Frames > 4900 {
		t.Fatalf("重采样帧数 = %d，期望约 4800", out.Frames)
	}
	if len(out.Samples) != out.Frames*2 {
		t.Fatal("输出必须为交错立体声")
	}
	// 单声道上混后两声道应一致。
	for i := 0; i < out.Frames; i++ {
		if out.Samples[i*2] != out.Samples[i*2+1] {
			t.Fatalf("帧 %d 两声道不一致", i)
		}
	}
}

func TestResampleRateChangeReconfigures(t *testing.T) {
	r := NewResampler()
	defer r.Close()
	if _, err := r.Convert(toneFrame(44100, 2, 1024, 300)); err != nil {
		t.Fatal(err)
	}
	// 输入签名变化（采样率改变）必须能无缝重建 swr。
	out, err := r.Convert(toneFrame(16000, 1, 800, 300))
	if err != nil {
		t.Fatalf("输入格式切换失败: %v", err)
	}
	if out.Frames <= 0 {
		t.Fatal("切换后无输出")
	}
}

func TestResampleDeterministic(t *testing.T) {
	run := func() int {
		r := NewResampler()
		defer r.Close()
		total := 0
		for i := 0; i < 5; i++ {
			out, err := r.Convert(toneFrame(44100, 2, 1024, 440))
			if err != nil {
				t.Fatal(err)
			}
			total += len(out.Samples)
		}
		return total
	}
	if run() != run() {
		t.Fatal("重采样结果不确定")
	}
}

func TestResampleFlushProducesTail(t *testing.T) {
	r := NewResampler()
	defer r.Close()
	for i := 0; i < 10; i++ {
		if _, err := r.Convert(toneFrame(44100, 2, 1024, 440)); err != nil {
			t.Fatal(err)
		}
	}
	tail, err := r.Flush()
	if err != nil {
		t.Fatal(err)
	}
	if tail.Frames < 0 {
		t.Fatal("冲刷帧数非法")
	}
	// 连续两次冲刷第二次应为空（幂等）。
	tail2, _ := r.Flush()
	if tail2.Frames != 0 {
		t.Fatalf("二次冲刷非空: %d", tail2.Frames)
	}
}

func TestSilenceAndAppend(t *testing.T) {
	s := Silence(100)
	if s.Frames != 100 || len(s.Samples) != 200 {
		t.Fatal("静音缓冲尺寸错误")
	}
	for _, v := range s.Samples {
		if v != 0 {
			t.Fatal("静音必须为零样本")
		}
	}
	a := &Buffer{Samples: make([]float32, 4), Frames: 2}
	b := &Buffer{Samples: make([]float32, 6), Frames: 3}
	c := Append(a, b)
	if c.Frames != 5 || len(c.Samples) != 10 {
		t.Fatal("拼接尺寸错误")
	}
}

func TestResampleEmptyFrame(t *testing.T) {
	r := NewResampler()
	defer r.Close()
	out, err := r.Convert(nil)
	if err != nil || out.Frames != 0 {
		t.Fatalf("空帧处理错误: %+v err=%v", out, err)
	}
}
