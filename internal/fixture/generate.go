// Package fixture 用确定性随机数生成自包含 fMP4 素材：正常组与四类损坏组
// （缺 mfhd、tfdt 回退、截断 box、未知声道布局），不引用任何外部文件。
package fixture

import (
	"bytes"
	"encoding/binary"
	"math/rand"

	"mediacheck/internal/model"
)

// Injection 标识损坏注入类型。
type Injection string

const (
	InjectNone           Injection = ""
	InjectMissingMFHD    Injection = "missing-mfhd"
	InjectTFDTRegression Injection = "tfdt-regression"
	InjectTruncatedBox   Injection = "truncated-box"
	InjectUnknownLayout  Injection = "unknown-layout"
)

// Bundle 是一组自包含素材：初始化段 + 分片字节 + 期望元数据。
type Bundle struct {
	InitSegment []byte
	Fragments   [][]byte
	Rate        uint32
	Layout      model.ChannelLayout
	Channels    uint16
	Seed        int64
	Injected    Injection
	// GapSeqs 记录人为制造的 mfhd 序号缺口（正常组也含一个可复核缺口）。
	GapSeqs []uint32
}

// Generate 按种子与注入类型生成素材。fragCount 个音频分片，每片 1 秒。
func Generate(seed int64, rate uint32, layout model.ChannelLayout, fragCount int, inject Injection) *Bundle {
	rng := rand.New(rand.NewSource(seed))
	ch := uint16(layout.Channels())
	b := &Bundle{Rate: rate, Layout: layout, Channels: ch, Seed: seed, Injected: inject}
	if inject == InjectUnknownLayout {
		ch = 3 // 非白名单声道数，触发未知布局
		b.Channels = 3
		b.Layout = ""
	}
	b.InitSegment = buildInit(rate, ch)
	// 正常组制造一个 mfhd 缺口：序号走 1,2,4,...（缺 3），用于 fragment_gaps=1 验收。
	seq := uint32(1)
	for i := 0; i < fragCount; i++ {
		if inject != InjectMissingMFHD || i != 0 {
			// 第二个分片起跳过序号 3，制造稳定缺口。
			if seq == 3 {
				b.GapSeqs = append(b.GapSeqs, 3)
				seq = 4
			}
		}
		tfdt := uint32(i) * rate
		if inject == InjectTFDTRegression && i == 1 {
			tfdt = 0 // 显式回退到起点
		}
		payload := genPCM(rng, int(rate), int(ch))
		frag := buildFragment(seq, rate, tfdt, payload)
		switch inject {
		case InjectMissingMFHD:
			if i == 0 {
				frag = removeMFHD(frag)
			}
		case InjectTruncatedBox:
			if i == 0 {
				frag = frag[:len(frag)-5] // 截断 mdat 尾部
			}
		}
		b.Fragments = append(b.Fragments, frag)
		seq++
	}
	return b
}

func genPCM(rng *rand.Rand, frames, channels int) []byte {
	out := make([]byte, frames*channels*2)
	for i := 0; i < frames*channels; i++ {
		v := int16(rng.Intn(4000) - 2000)
		binary.LittleEndian.PutUint16(out[i*2:], uint16(v))
	}
	return out
}

// ---- 字节级 box 构造（与适配器/再封装约定一致） ----

func box(typ string, payload []byte) []byte {
	out := make([]byte, 8+len(payload))
	binary.BigEndian.PutUint32(out[:4], uint32(8+len(payload)))
	copy(out[4:8], typ)
	copy(out[8:], payload)
	return out
}

func fullBox(typ string, flags uint32, fields []byte) []byte {
	body := make([]byte, 4+len(fields))
	binary.BigEndian.PutUint32(body[:4], flags)
	copy(body[4:], fields)
	return box(typ, body)
}

func u32(v uint32) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, v)
	return b
}

func buildInit(rate uint32, channels uint16) []byte {
	var b bytes.Buffer
	b.Write(box("ftyp", append([]byte("iso5"), make([]byte, 9)...)))
	tkhdFields := make([]byte, 12)
	binary.BigEndian.PutUint32(tkhdFields[8:12], 1)
	tkhd := fullBox("tkhd", 7, tkhdFields)
	mdhdFields := make([]byte, 16)
	binary.BigEndian.PutUint32(mdhdFields[8:12], rate)
	mdhd := fullBox("mdhd", 0, mdhdFields)
	hdlr := fullBox("hdlr", 0, append(make([]byte, 4), []byte("soun")...))
	entryBody := make([]byte, 20)
	binary.BigEndian.PutUint16(entryBody[8:10], channels)
	binary.BigEndian.PutUint16(entryBody[10:12], 16)
	binary.BigEndian.PutUint32(entryBody[16:20], rate<<16)
	stsdFields := append(u32(1), box("ipcm", entryBody)...)
	stsd := fullBox("stsd", 0, stsdFields)
	mdia := box("mdia", append(append(mdhd, hdlr...), box("minf", stsd)...))
	b.Write(box("moov", box("trak", append(tkhd, mdia...))))
	return b.Bytes()
}

func buildFragment(seq, rate, tfdt uint32, payload []byte) []byte {
	mfhd := fullBox("mfhd", 0, u32(seq))
	tfhd := fullBox("tfhd", 0, u32(1))
	tfdtBox := fullBox("tfdt", 0, u32(tfdt))
	flags := uint32(0x000001 | 0x000100 | 0x000200)
	trunFields := append(u32(1), u32(0)...)
	trunFields = append(trunFields, u32(rate)...)
	trunFields = append(trunFields, u32(uint32(len(payload)))...)
	trun := fullBox("trun", flags, trunFields)
	traf := box("traf", append(append(tfhd, tfdtBox...), trun...))
	moof := box("moof", append(mfhd, traf...))
	dataOffPos := len(moof) - len(trun) + 8 + 4 + 4
	binary.BigEndian.PutUint32(moof[dataOffPos:dataOffPos+4], uint32(len(moof)+8))
	return append(moof, box("mdat", payload)...)
}

// removeMFHD 重建一个不含 mfhd 的 moof，保留 traf，制造“分片无 mfhd”。
// 末尾 mdat 原样保留，data_offset 因 moof 变短而重新计算。
func removeMFHD(frag []byte) []byte {
	mdatIdx := bytes.Index(frag, []byte("mdat"))
	if mdatIdx < 4 {
		return frag
	}
	trafIdx := bytes.Index(frag, []byte("traf"))
	if trafIdx < 4 {
		return frag
	}
	trafSize := binary.BigEndian.Uint32(frag[trafIdx-4 : trafIdx])
	traf := frag[trafIdx-4 : trafIdx-4+int(trafSize)]
	moof := box("moof", traf)
	// 回填 trun data_offset：新 moof 长度 + mdat 8 字节头。
	if trPos := bytes.Index(moof, []byte("trun")); trPos >= 4 {
		dataOffPos := trPos - 4 + 8 + 4 + 4
		if dataOffPos+4 <= len(moof) {
			binary.BigEndian.PutUint32(moof[dataOffPos:dataOffPos+4], uint32(len(moof)+8))
		}
	}
	out := append([]byte{}, moof...)
	out = append(out, frag[mdatIdx-4:]...)
	return out
}
