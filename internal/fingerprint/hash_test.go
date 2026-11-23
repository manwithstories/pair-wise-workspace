package fingerprint_test

import (
	"testing"

	"media-pipeline/internal/clock"
	"media-pipeline/internal/media"
	"media-pipeline/internal/fingerprint"
)

var tb = clock.Timebase{SampleRate: 8000, Channels: 2, BitsPerSample: 16}

// pcm 生成确定性采样：每帧 4 字节（立体声 16bit）。
func pcm(n int, seed byte) []byte {
	buf := make([]byte, n*tb.FrameSize())
	for i := range buf {
		buf[i] = seed + byte(i%7)
	}
	return buf
}

func runHasher(t *testing.T, blocks []media.Block) ([]fingerprint.Fingerprint, []fingerprint.Duplicate) {
	t.Helper()
	h := fingerprint.New(tb, 4) // 4 帧一个窗口
	var fps []fingerprint.Fingerprint
	var first uint64
	for i, b := range blocks {
		b.FirstSample = first
		b.Format = tb
		if b.ID == "" {
			b.ID = string(rune('A' + i))
		}
		got, err := h.Push(b)
		if err != nil {
			t.Fatalf("push %d: %v", i, err)
		}
		fps = append(fps, got...)
		n, _ := b.FrameCount()
		first += n
	}
	return fps, h.Duplicates()
}

func TestCrossBoundaryFingerprintStable(t *testing.T) {
	data := pcm(10, 3)
	whole, _ := runHasher(t, []media.Block{{PCM: data}})
	split, _ := runHasher(t, []media.Block{
		{PCM: data[:3*tb.FrameSize()]},
		{PCM: data[3*tb.FrameSize():]},
	})
	choppy, _ := runHasher(t, []media.Block{
		{PCM: data[:1*tb.FrameSize()]},
		{PCM: data[1*tb.FrameSize():5*tb.FrameSize()]},
		{PCM: data[5*tb.FrameSize():]},
	})
	if len(whole) != len(split) || len(split) != len(choppy) {
		t.Fatalf("window counts differ: %d %d %d", len(whole), len(split), len(choppy))
	}
	for i := range whole {
		if whole[i].Sum != split[i].Sum || split[i].Sum != choppy[i].Sum {
			t.Fatalf("window %d fingerprint differs across block boundaries", i)
		}
		if whole[i].FirstSample != split[i].FirstSample {
			t.Fatalf("window %d sample origin differs", i)
		}
	}
}

func TestDuplicateDetectedWithFirstSource(t *testing.T) {
	data := pcm(8, 5) // 8 帧内容，窗口大小 4
	// src-1 提供首次出现的窗口，src-2 携带相同内容的重复窗口。
	fps, dups := runHasher(t, []media.Block{
		{ID: "src-1", PCM: data[:4*tb.FrameSize()]},
		{ID: "src-2", PCM: data[4*tb.FrameSize():8*tb.FrameSize()]},
		{ID: "src-2", PCM: data[:4*tb.FrameSize()]}, // 逐字节复制 src-1
	})
	if len(fps) == 0 {
		t.Fatal("no fingerprints produced")
	}
	if len(dups) == 0 {
		t.Fatal("duplicate content not detected")
	}
	for _, d := range dups {
		if d.FirstSource != "src-1" || d.RepeatSource != "src-2" {
			t.Fatalf("duplicate provenance wrong: %+v", d)
		}
		if d.RepeatSample <= d.FirstSample {
			t.Fatalf("repeat sample %d must follow first sample %d", d.RepeatSample, d.FirstSample)
		}
	}
}

func TestDistinctContentDistinctHash(t *testing.T) {
	a, _ := runHasher(t, []media.Block{{PCM: pcm(8, 1)}})
	b, _ := runHasher(t, []media.Block{{PCM: pcm(8, 9)}})
	if a[0].Sum == b[0].Sum {
		t.Fatal("different PCM produced identical fingerprint")
	}
}

func TestCanonicalizationAcrossBitDepth(t *testing.T) {
	// 同样的逻辑采样值，16bit 与（补齐后的）规范化结果应一致。
	tb16 := clock.Timebase{SampleRate: 8000, Channels: 1, BitsPerSample: 16}
	tb24 := clock.Timebase{SampleRate: 8000, Channels: 1, BitsPerSample: 24}
	v16 := []byte{0x80, 0x00} // +128 小端 16bit
	v24 := []byte{0x80, 0x00, 0x00}
	h16 := fingerprint.New(tb16, 1)
	h24 := fingerprint.New(tb24, 1)
	f16, err := h16.Push(media.Block{ID: "a", Format: tb16, PCM: v16})
	if err != nil {
		t.Fatal(err)
	}
	f24, err := h24.Push(media.Block{ID: "b", Format: tb24, PCM: v24})
	if err != nil {
		t.Fatal(err)
	}
	if f16[0].Sum != f24[0].Sum {
		t.Fatalf("canonical hashes differ for equal logical samples: %x vs %x", f16[0].Sum, f24[0].Sum)
	}
}
