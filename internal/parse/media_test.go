package parse

import (
	"bytes"
	"testing"

	"github.com/abema/go-mp4"
)

// fullbox 给需要 version/flags 的盒补 4 字节 FullBox 头。
func fullbox(payload []byte) []byte { return append(make([]byte, 4), payload...) }

func u32(v uint32) []byte {
	return []byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)}
}

// avccSample 构造 AVCC 长度前缀的 H.264 样本，nalType 5=IDR，1=非 IDR。
func avccSample(nalType byte, payloadLen int) []byte {
	nal := append([]byte{nalType}, make([]byte, payloadLen)...)
	return append(u32(uint32(len(nal))), nal...)
}

// buildMiniMP4 构造单视频轨（avc1）双样本的最小容器。
// withStss=false 时省略关键帧索引以模拟索引缺页。
func buildMiniMP4(t *testing.T, withStss bool) ([]byte, []byte, []byte) {
	s1 := avccSample(5, 8)
	s2 := avccSample(1, 8)
	mdatPayload := append(append([]byte{}, s1...), s2...)
	off1 := uint32(0) // 占位，写 mdat 后回填
	_ = off1

	stsd := box("stsd", fullbox(append(u32(1), box("avc1", make([]byte, 78))...)))
	stts := box("stts", fullbox(append(u32(1),
		append(u32(2), u32(1000)...)...))) // 1 条目：2 样本各 1000 tick
	// 单 chunk 含 2 个样本：first_chunk=1, samples_per_chunk=2, desc_idx=1。
	stsc := box("stsc", fullbox(append(u32(1),
		append(u32(1), append(u32(2), u32(1)...)...)...)))
	stsz := box("stsz", fullbox(append(append(u32(0), u32(2)...),
		append(u32(uint32(len(s1))), u32(uint32(len(s2)))...)...)))
	stblChildren := [][]byte{stsd, stts, stsc, stsz,
		box("stco", fullbox(append(u32(1), u32(0)...)))} // 1 个 chunk，偏移占位
	if withStss {
		stblChildren = append(stblChildren,
			box("stss", fullbox(append(u32(1), u32(1)...))))
	}
	var stblPayload []byte
	for _, c := range stblChildren {
		stblPayload = append(stblPayload, c...)
	}
	minf := nested("minf", nested("stbl", stblPayload))
	mdia := nested("mdia",
		box("mdhd", fullbox(append(append(u32(0), u32(0)...),
			append(append(u32(90000), u32(2000)...), make([]byte, 4)...)...))),
		box("hdlr", fullbox(append(append(make([]byte, 4),
			[]byte("vide")...), append(make([]byte, 12), []byte("v")...)...))),
		minf)
	tkhd := box("tkhd", fullbox(append(append(make([]byte, 8),
		u32(1)...), append(make([]byte, 8), make([]byte, 80)...)...)))
	trak := nested("trak", tkhd, mdia)
	moov := nested("moov",
		box("mvhd", fullbox(make([]byte, 96))), trak)

	var out bytes.Buffer
	out.Write(box("ftyp", append([]byte("isom"), make([]byte, 4)...)))
	mdatHeaderPos := out.Len()
	out.Write(box("mdat", mdatPayload))
	sampleBase := uint32(mdatHeaderPos + 8)
	out.Write(moov)
	raw := out.Bytes()

	// 回填 stco：样本紧跟 mdat 头。
	stcoBox := findIn(raw, "stco")
	if stcoBox < 0 {
		t.Fatal("stco not found")
	}
	// findIn 返回盒起始偏移。首个 chunk offset 位于：
	// 盒头(8) + FullBox(4) + entry_count(4) = 起始 +16。
	p := stcoBox + 16
	copy(raw[p:p+4], u32(sampleBase))
	return raw, s1, s2
}

// findIn 在原始字节中查找盒类型，返回盒起始偏移。
func findIn(data []byte, typ string) int {
	needle := []byte(typ)
	for i := 0; i+8 <= len(data); i++ {
		if bytes.Equal(data[i+4:i+8], needle) {
			return i
		}
	}
	return -1
}

func parseFirstTrack(t *testing.T, data []byte) ([]*TrackInfo, *Report) {
	roots, rep := Walk(bytes.NewReader(data), DefaultLimits())
	if rep.HasFatal() {
		t.Fatalf("walk fatal: %v", rep.Errors)
	}
	tis, err := ParseTracks(bytes.NewReader(data), roots, DefaultLimits(), rep)
	if err != nil {
		t.Fatalf("parse tracks: %v", err)
	}
	if len(tis) != 1 {
		for _, e := range rep.Errors {
			t.Logf("report: %v", e)
		}
		t.Fatalf("want 1 track, got %d", len(tis))
	}
	return tis, rep
}

func TestParseSampleTable(t *testing.T) {
	data, s1, s2 := buildMiniMP4(t, true)
	tis, rep := parseFirstTrack(t, data)
	ti := tis[0]
	if ti.Kind != "video" || ti.Codec != "avc1" || ti.Timescale != 90000 {
		t.Fatalf("track meta wrong: %+v", ti)
	}
	if len(ti.Samples) != 2 {
		t.Fatalf("samples=%d", len(ti.Samples))
	}
	if ti.Samples[0].Size != uint32(len(s1)) || ti.Samples[1].Size != uint32(len(s2)) {
		t.Fatal("sample sizes wrong")
	}
	if ti.Samples[0].RawTick != 0 || ti.Samples[1].RawTick != 1000 {
		t.Fatalf("raw ticks wrong: %d %d", ti.Samples[0].RawTick, ti.Samples[1].RawTick)
	}
	if ti.Samples[0].TimeNs != 0 || ti.Samples[1].TimeNs <= ti.Samples[0].TimeNs {
		t.Fatal("timeline mapping wrong")
	}
	if !ti.Samples[0].SyncKnown || !ti.Samples[0].Sync || ti.Samples[1].Sync {
		t.Fatalf("stss sync flags wrong: %+v", ti.Samples)
	}
	if ti.IndexGap || rep.TimelineErrors() != 0 {
		t.Fatal("healthy input must have no gap/timeline errors")
	}
}

func TestRebuildIndexFromGap(t *testing.T) {
	data, _, _ := buildMiniMP4(t, false) // 无 stss
	tis, rep := parseFirstTrack(t, data)
	ti := tis[0]
	if !ti.IndexGap {
		t.Fatal("missing stss must flag index gap")
	}
	RebuildKeyframeIndex(bytes.NewReader(data), ti, DefaultLimits(), rep)
	if ti.IndexGap {
		t.Fatal("index should be rebuilt from payload scan")
	}
	if !ti.Samples[0].SyncKnown || !ti.Samples[0].Sync {
		t.Fatal("first sample IDR must be recovered")
	}
	if !ti.Samples[1].SyncKnown || ti.Samples[1].Sync {
		t.Fatal("non-IDR sample must not be marked sync")
	}
}

func TestUnsupportedCodecBoundariesUnsafe(t *testing.T) {
	data, _, _ := buildMiniMP4(t, false)
	// 把 avc1 改成未支持编码，重建不得猜测随机访问点。
	p := findIn(data, "avc1")
	copy(data[p+4:p+8], []byte("vp09"))
	tis, rep := parseFirstTrack(t, data)
	ti := tis[0]
	RebuildKeyframeIndex(bytes.NewReader(data), ti, DefaultLimits(), rep)
	for _, s := range ti.Samples {
		if s.SyncKnown {
			t.Fatalf("unknown codec boundary must be unsafe: %+v", s)
		}
	}
	if rep.SkippedBoxes == 0 {
		t.Fatal("unsupported codec must be explicitly reported, not silent")
	}
}

func TestUnsupportedHandlerReported(t *testing.T) {
	data, _, _ := buildMiniMP4(t, true)
	// hdlr 净荷布局：FullBox(4) + predefined(4) + handlerType(4)。
	hdlrPos := findIn(data, "hdlr")
	if hdlrPos < 0 {
		t.Fatal("hdlr not found")
	}
	copy(data[hdlrPos+8+8:hdlrPos+8+12], []byte("hint"))
	roots, rep := Walk(bytes.NewReader(data), DefaultLimits())
	tis, err := ParseTracks(bytes.NewReader(data), roots, DefaultLimits(), rep)
	if err != nil {
		t.Fatal(err)
	}
	if len(tis) != 0 {
		t.Fatal("unsupported handler track must not be emitted as parseable")
	}
	if rep.SkippedBoxes == 0 {
		t.Fatal("unsupported handler must be reported")
	}
}

// 确保 go-mp4 类型在测试辅助中被引用，避免导入漂移。
var _ = mp4.BoxTypeStss
