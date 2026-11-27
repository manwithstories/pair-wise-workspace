package ingest

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"

	"github.com/go-audio/riff"

	"bcastqc/internal/model"
)

// wavBuilder 用于在内存中拼装合法/畸形 WAV 字节。
type wavBuilder struct {
	buf     bytes.Buffer
	fmtData []byte
}

func newWavBuilder(sampleRate uint32, chans, bits uint16, format uint16) *wavBuilder {
	wb := &wavBuilder{}
	wb.buf.Write(riff.RiffID[:])
	binary.Write(&wb.buf, binary.LittleEndian, uint32(0)) // 占位 RIFF size
	wb.buf.Write(riff.WavFormatID[:])
	blockAlign := chans * bits / 8
	fmt := struct {
		Format      uint16
		NumChans    uint16
		SampleRate  uint32
		BytesPerSec uint32
		BlockAlign  uint16
		Bits        uint16
	}{format, chans, sampleRate, sampleRate * uint32(blockAlign), blockAlign, bits}
	var fb bytes.Buffer
	binary.Write(&fb, binary.LittleEndian, &fmt)
	wb.fmtData = fb.Bytes()
	return wb
}

func (wb *wavBuilder) writeChunk(id [4]byte, payload []byte, declaredSize uint32) {
	wb.buf.Write(id[:])
	binary.Write(&wb.buf, binary.LittleEndian, declaredSize)
	wb.buf.Write(payload)
	if declaredSize%2 == 1 {
		wb.buf.WriteByte(0) // padding
	}
}

func (wb *wavBuilder) addFmt() { wb.writeChunk(riff.FmtID, wb.fmtData, uint32(len(wb.fmtData))) }

func (wb *wavBuilder) addData(pcm []byte) { wb.writeChunk(riff.DataFormatID, pcm, uint32(len(pcm))) }

func (wb *wavBuilder) addAncillary(id [4]byte, payload []byte) { wb.writeChunk(id, payload, uint32(len(payload))) }

func (wb *wavBuilder) bytes() []byte {
	out := wb.buf.Bytes()
	binary.LittleEndian.PutUint32(out[4:], uint32(len(out)-8))
	return out
}

func validPCM(frames int) []byte {
	pcm := make([]byte, frames*2) // 16bit 单声道，每帧 2 字节
	for i := range pcm {
		pcm[i] = byte(i % 251)
	}
	return pcm
}

func TestParseWAVValidWithOutOfOrderChunks(t *testing.T) {
	wb := newWavBuilder(48000, 1, 16, WaveFormatPCM)
	// 附属块出现在 fmt 之前、data 之后，验证任意顺序容忍。
	wb.addAncillary([4]byte{'L', 'I', 'S', 'T'}, []byte("INFOextra"))
	wb.addFmt()
	wb.addAncillary([4]byte{'b', 'e', 'x', 't'}, []byte("broadcast metadata"))
	pcm := validPCM(100)
	wb.addData(pcm)
	raw := wb.bytes()

	info, err := ParseWAV(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		t.Fatalf("合法 WAV 解析失败: %v", err)
	}
	if info.SampleRate != 48000 || info.NumChans != 1 || info.BitsPerSample != 16 {
		t.Fatalf("格式参数错误: %+v", info)
	}
	if info.BlockAlign != 2 || info.TotalFrames != 100 || info.DataBytes != 200 {
		t.Fatalf("长度推算错误: %+v", info)
	}
	if len(info.Chunks) != 4 {
		t.Fatalf("应枚举到 4 个子块，实际 %d: %+v", len(info.Chunks), info.Chunks)
	}
	if info.Chunks[0].ID != "LIST" || info.Chunks[3].ID != "data" {
		t.Fatalf("子块顺序记录错误: %+v", info.Chunks)
	}
}

func TestParseWAVRejectsBadMagic(t *testing.T) {
	raw := make([]byte, 64)
	if _, err := ParseWAV(bytes.NewReader(raw), int64(len(raw))); !errors.Is(err, model.ErrRiffStructure) {
		t.Fatalf("非法魔数应报 ErrRiffStructure，实际 %v", err)
	}
}

func TestParseWAVRejectsUnsupportedCodec(t *testing.T) {
	wb := newWavBuilder(48000, 1, 16, 3) // IEEE float 标签
	wb.addFmt()
	wb.addData(validPCM(10))
	raw := wb.bytes()
	if _, err := ParseWAV(bytes.NewReader(raw), int64(len(raw))); !errors.Is(err, model.ErrUnsupportedCodec) {
		t.Fatalf("非 PCM 编码应报 ErrUnsupportedCodec，实际 %v", err)
	}
}

func TestParseWAVRejectsRiffLengthOverflow(t *testing.T) {
	wb := newWavBuilder(48000, 1, 16, WaveFormatPCM)
	wb.addFmt()
	wb.addData(validPCM(10))
	raw := wb.bytes()
	// 篡改 RIFF 主块长度为远超实际文件。
	binary.LittleEndian.PutUint32(raw[4:], 0xFFFFFFFF)
	if _, err := ParseWAV(bytes.NewReader(raw), int64(len(raw))); !errors.Is(err, model.ErrRiffStructure) {
		t.Fatalf("RIFF 长度溢出应报 ErrRiffStructure，实际 %v", err)
	}
}

func TestParseWAVRejectsChunkOverflow(t *testing.T) {
	wb := newWavBuilder(48000, 1, 16, WaveFormatPCM)
	wb.addFmt()
	// data 声明 9999 字节但只给 4 字节。
	wb.writeChunk(riff.DataFormatID, []byte{0, 0, 0, 0}, 9999)
	raw := wb.bytes()
	if _, err := ParseWAV(bytes.NewReader(raw), int64(len(raw))); !errors.Is(err, model.ErrRiffStructure) {
		t.Fatalf("子块越界应报 ErrRiffStructure，实际 %v", err)
	}
}

func TestParseWAVRejectsTruncatedData(t *testing.T) {
	wb := newWavBuilder(48000, 1, 16, WaveFormatPCM)
	wb.addFmt()
	wb.writeChunk(riff.DataFormatID, []byte{1, 2, 3}, 3) // 奇数且非块对齐
	raw := wb.bytes()
	if _, err := ParseWAV(bytes.NewReader(raw), int64(len(raw))); !errors.Is(err, model.ErrRiffStructure) {
		t.Fatalf("截断 data 应报 ErrRiffStructure，实际 %v", err)
	}
}

func TestParseWAVOddLengthPadding(t *testing.T) {
	wb := newWavBuilder(8000, 1, 8, WaveFormatPCM)
	wb.addFmt()
	pcm := []byte{1, 2, 3} // 3 字节，奇数，带 padding
	wb.writeChunk(riff.DataFormatID, pcm, 3)
	raw := wb.bytes()
	info, err := ParseWAV(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		t.Fatalf("奇数长度带 padding 应可解析: %v", err)
	}
	if info.DataBytes != 3 || info.TotalFrames != 3 {
		t.Fatalf("奇数 data 长度记录错误: %+v", info)
	}
}

func mkPacket(seq, start, frames int64, anchor bool, payload byte) Packet {
	return Packet{
		TrackID: 1, Seq: seq, StartSample: start, Frames: frames,
		Anchor: anchor, Payload: []byte{payload}, CRC: 0,
	}
}

func TestWindowDedupAndConflict(t *testing.T) {
	a := NewWindowAssembler(1, 4)
	p1 := mkPacket(0, 0, 10, true, 7)
	if err := a.AddPacket(p1); err != nil {
		t.Fatalf("首包: %v", err)
	}
	if err := a.AddPacket(p1); err != nil { // 完全重复
		t.Fatalf("相同重复包应幂等忽略，实际 %v", err)
	}
	if a.Stats().Duplicates != 1 {
		t.Fatalf("重复计数应为 1，实际 %d", a.Stats().Duplicates)
	}
	p1diff := p1
	p1diff.Payload = []byte{9}
	err := a.AddPacket(p1diff)
	if !errors.Is(err, model.ErrPacketConflict) {
		t.Fatalf("同序号异内容应报冲突，实际 %v", err)
	}
	if a.Stats().Conflicts != 1 {
		t.Fatalf("冲突计数应为 1，实际 %d", a.Stats().Conflicts)
	}
}

func TestWindowReorderAndBurstLoss(t *testing.T) {
	a := NewWindowAssembler(1, 2)
	// 乱序到达：0,2,1，窗口 2 内容纳。
	must := func(err error) { t.Helper(); if err != nil { t.Fatal(err) } }
	must(a.AddPacket(mkPacket(0, 0, 10, true, 0)))
	must(a.AddPacket(mkPacket(2, 20, 10, false, 2)))
	must(a.AddPacket(mkPacket(1, 10, 10, false, 1)))
	recs, stats := a.Flush()
	if len(recs) != 3 || recs[0].Seq != 0 || recs[1].Seq != 1 || recs[2].Seq != 2 {
		t.Fatalf("乱序重排结果错误: %+v", recs)
	}
	if stats.Reordered == 0 {
		t.Fatal("应记录乱序包数量")
	}

	// 突发丢失：窗口 2，期望序号从 0 起，3/4/5 到达迫使 0、1 判丢。
	b := NewWindowAssembler(1, 2)
	must(b.AddPacket(mkPacket(3, 30, 10, false, 3)))
	must(b.AddPacket(mkPacket(4, 40, 10, true, 4)))
	must(b.AddPacket(mkPacket(5, 50, 10, false, 5)))
	_, stats = b.Flush()
	lost := stats.LostSeqs[1]
	if len(lost) != 3 || lost[0] != 0 || lost[1] != 1 || lost[2] != 2 {
		t.Fatalf("序号 0/1/2 应被判突发丢失，实际 %+v", lost)
	}
}

func TestWindowMemoryBoundedByWindow(t *testing.T) {
	a := NewWindowAssembler(1, 3)
	for seq := int64(0); seq < 100; seq += 2 { // 只发偶数序号
		if err := a.AddPacket(mkPacket(seq, seq*10, 10, false, byte(seq))); err != nil {
			t.Fatal(err)
		}
		if a.PendingCount() > 4 { // 窗口 3 加一个在途，缓存不应随总长增长
			t.Fatalf("缓存包数 %d 超出窗口上界，内存随素材总长增长", a.PendingCount())
		}
	}
	recs, _ := a.Flush()
	if len(recs) != 50 {
		t.Fatalf("应释放 50 个偶数包，实际 %d", len(recs))
	}
}

func TestBandwidthPlanDelivery(t *testing.T) {
	plan, err := NewBandwidthPlan([]int64{1_200_000, 240_000, 900_000}, 1000, 120_000)
	if err != nil {
		t.Fatal(err)
	}
	// 每片容量：10 包 / 2 包 / 7 包。
	if c := plan.CapacityAt(0); c != 10 {
		t.Fatalf("高档位容量应为 10，实际 %d", c)
	}
	if c := plan.CapacityAt(1000); c != 2 {
		t.Fatalf("低档位容量应为 2，实际 %d", c)
	}
	if c := plan.CapacityAt(2000); c != 7 {
		t.Fatalf("中档位容量应为 7，实际 %d", c)
	}
	pkts := make([]Packet, 20)
	for i := range pkts {
		pkts[i] = mkPacket(int64(i), int64(i)*10, 10, i%5 == 0, byte(i))
	}
	// 每片注入 3 包、缓存仅容 2 包：低带宽档（容量 2 < 注入 3）必然积压丢包。
	arrived := plan.SimulateDelivery(pkts, 2, 3)
	if len(arrived) >= len(pkts) {
		t.Fatalf("小缓存 + 低带宽应产生丢失，到达 %d / 总 %d", len(arrived), len(pkts))
	}
	// 到达结果应保持相对有序（队列 FIFO 放行）。
	for i := 1; i < len(arrived); i++ {
		if arrived[i].Seq < arrived[i-1].Seq {
			t.Fatalf("放行顺序破坏 FIFO: %d 在 %d 后", arrived[i].Seq, arrived[i-1].Seq)
		}
	}
}

func TestBandwidthPlanRejectsInvalid(t *testing.T) {
	if _, err := NewBandwidthPlan(nil, 10, 100); !errors.Is(err, model.ErrInvalidInput) {
		t.Fatalf("空计划应报错: %v", err)
	}
	if _, err := NewBandwidthPlan([]int64{100}, 0, 100); !errors.Is(err, model.ErrInvalidInput) {
		t.Fatalf("零时间片应报错: %v", err)
	}
	if _, err := NewBandwidthPlan([]int64{-1}, 10, 100); !errors.Is(err, model.ErrInvalidInput) {
		t.Fatalf("负带宽应报错: %v", err)
	}
}

func TestContentHashStableAndSensitive(t *testing.T) {
	p := mkPacket(1, 10, 5, true, 42)
	h1 := p.ContentHash()
	h2 := p.ContentHash()
	if h1 != h2 {
		t.Fatal("相同内容哈希不稳定")
	}
	p2 := p
	p2.Payload = []byte{43}
	if p2.ContentHash() == h1 {
		t.Fatal("载荷变化后哈希应不同")
	}
	// 序号不参与哈希（同序号异内容由上层比对）。
	p3 := p
	p3.Seq = 99
	if p3.ContentHash() != h1 {
		t.Fatal("哈希不应包含序号字段")
	}
}

func TestParseWAVShortFile(t *testing.T) {
	if _, err := ParseWAV(bytes.NewReader([]byte("RIFF")), 4); !errors.Is(err, model.ErrRiffStructure) {
		t.Fatalf("过短文件应报 ErrRiffStructure: %v", err)
	}
	if _, err := ParseWAV(bytes.NewReader(nil), 0); !errors.Is(err, model.ErrRiffStructure) {
		t.Fatalf("零长度文件应报结构错误，实际 %v", err)
	}
}
