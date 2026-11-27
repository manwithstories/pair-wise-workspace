package timeline

import (
	"testing"

	"medpipe/internal/clock"
	"medpipe/internal/ingest"
)

func pcmPacket(seq, start, frames, block int64, src ingest.Source, values ...int16) *ingest.Packet {
	ch := len(values)
	buf := make([]byte, int(frames)*ch*2)
	for f := int64(0); f < frames; f++ {
		for c, v := range values {
			off := (f*int64(ch) + int64(c)) * 2
			buf[off] = byte(uint16(v))
			buf[off+1] = byte(uint16(v) >> 8)
		}
	}
	return &ingest.Packet{Seq: seq, BlockID: block, StartFrame: start, Frames: frames,
		Source: src, SHA256: ingest.HashPacket(buf), PCM: buf}
}

func TestMixerPlaceAndExport(t *testing.T) {
	tb, _ := clock.NewTimeBase(48000)
	m, err := NewMixer(tb, 2, 16, 4)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Apply(pcmPacket(0, 0, 2, 0, ingest.SourcePCM, 100, -200)); err != nil {
		t.Fatal(err)
	}
	if err := m.Apply(pcmPacket(1, 2, 2, 1, ingest.SourcePCM, 300, -400)); err != nil {
		t.Fatal(err)
	}
	out := m.PCM()
	if len(out) != 4*2*2 {
		t.Fatalf("PCM 长度错误: %d", len(out))
	}
	// 帧 0 两声道位于 0/2；帧 3 第 1 声道位于 (3*2+1)*2 = 14
	if readSample(out, 0) != 100 || readSample(out, 2) != -200 || readSample(out, 14) != -400 {
		t.Fatalf("放置坐标错误: %d %d %d", readSample(out, 0), readSample(out, 2), readSample(out, 14))
	}
	clip, miss, align, ranges := m.Stats()
	if clip != 0 || miss != 0 || align != 0 || len(ranges) != 2 {
		t.Fatalf("统计异常 clip=%d miss=%d align=%d ranges=%d", clip, miss, align, len(ranges))
	}
}

func TestMixerSilenceAndNoDrift(t *testing.T) {
	tb, _ := clock.NewTimeBase(48000)
	m, _ := NewMixer(tb, 2, 16, 6)
	// 第一个块缺失，直接以静音占位 [0,2)，真实块仍落在 [2,4) 不漂移
	sil := &ingest.Packet{Seq: 0, BlockID: 0, StartFrame: 0, Frames: 2,
		Source: ingest.SourceSilence, PCM: make([]byte, 2*2*2)}
	if err := m.Apply(sil); err != nil {
		t.Fatal(err)
	}
	if err := m.Apply(pcmPacket(1, 2, 2, 1, ingest.SourcePCM, 50, 60)); err != nil {
		t.Fatal(err)
	}
	out := m.PCM()
	// 2 声道交错：帧 2 第 0 声道位于字节偏移 2帧*2ch*2B = 8
	if readSample(out, 0) != 0 || readSample(out, 8) != 50 {
		t.Fatalf("静音补偿后坐标漂移: %d %d", readSample(out, 0), readSample(out, 8))
	}
	_, miss, _, ranges := m.Stats()
	if miss != 4 { // 2 帧 * 2 声道
		t.Fatalf("缺失样本统计应为 4, 得 %d", miss)
	}
	if ranges[0].Source != ingest.SourceSilence || ranges[0].BlockID != 0 {
		t.Fatalf("静音来源未记录: %+v", ranges[0])
	}
}

func TestMixerClipping(t *testing.T) {
	tb, _ := clock.NewTimeBase(48000)
	m, _ := NewMixer(tb, 1, 16, 2)
	// 两次叠加 30000 应饱和到 32767
	_ = m.Apply(pcmPacket(0, 0, 2, 0, ingest.SourcePCM, 30000))
	_ = m.Apply(pcmPacket(0, 0, 2, 0, ingest.SourcePCM, 30000))
	out := m.PCM()
	if readSample(out, 0) != 32767 {
		t.Fatalf("正饱和失败: %d", readSample(out, 0))
	}
	// 满量程样本本身计入削波
	m2, _ := NewMixer(tb, 1, 16, 1)
	_ = m2.Apply(pcmPacket(0, 0, 1, 0, ingest.SourcePCM, -32768))
	clip, _, _, _ := m2.Stats()
	if clip == 0 {
		t.Fatal("满量程样本应计入削波")
	}
	clip, _, _, _ = m.Stats()
	if clip == 0 {
		t.Fatal("饱和削波未统计")
	}
}

func TestMixerRejectOutOfBounds(t *testing.T) {
	tb, _ := clock.NewTimeBase(48000)
	m, _ := NewMixer(tb, 1, 16, 2)
	bad := pcmPacket(5, 4, 2, 5, ingest.SourcePCM, 1)
	if err := m.Apply(bad); clock.CodeOf(err) != clock.CodeLatePacket {
		t.Fatalf("越界包应按 late_packet 拒绝, 得 %v", err)
	}
}
