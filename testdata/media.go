package testdata

import (
	"bytes"

	"github.com/abema/go-mp4"
)

// MediaSpec 描述一个机位的媒体参数。
type MediaSpec struct {
	Camera      string
	VideoFrames int
	IDREvery    int
	Timescale   uint32
	WithSTSS    bool
}

// DefaultSpecs 返回两台机位的默认规格（不同时间基准）。
func DefaultSpecs() []MediaSpec {
	return []MediaSpec{
		{Camera: "camA", VideoFrames: 24, IDREvery: 6, Timescale: 90000, WithSTSS: true},
		{Camera: "camB", VideoFrames: 20, IDREvery: 5, Timescale: 30000, WithSTSS: false},
	}
}

// BuildVideoSample 生成单个 AVCC H.264 样本，idr 决定 NAL 类型（5/1）。
func BuildVideoSample(g *RNG, idr bool) []byte {
	nalType := byte(1)
	if idr {
		nalType = 5
	}
	nal := append([]byte{nalType}, g.Byte(20+g.Intn(12))...)
	return append(U32b(uint32(len(nal))), nal...)
}

// BuildPCM 生成确定性 PCM 帧：静音区全零，其余低幅/高幅交替。
func BuildPCM(g *RNG, samples int, silent bool) []int16 {
	out := make([]int16, samples)
	if silent {
		return out
	}
	for i := range out {
		out[i] = int16(8000 + g.Intn(20000))
	}
	return out
}

// BuildMP4 构造单视频轨 MP4（avc1），样本内联单个 mdat。
// 返回容器字节与样本净荷（供调用方生成损坏变体）。
func BuildMP4(g *RNG, spec MediaSpec) ([]byte, [][]byte) {
	samples := make([][]byte, spec.VideoFrames)
	sizes := make([]uint32, spec.VideoFrames)
	var mdatPayload []byte
	for i := 0; i < spec.VideoFrames; i++ {
		samples[i] = BuildVideoSample(g, i%spec.IDREvery == 0)
		sizes[i] = uint32(len(samples[i]))
		mdatPayload = append(mdatPayload, samples[i]...)
	}

	// 样本表：全部样本在单个 chunk，等间隔 delta。
	delta := spec.Timescale / 10 // 10fps
	sttsEntries := U32b(1)
	sttsEntries = append(sttsEntries, U32b(uint32(spec.VideoFrames))...)
	sttsEntries = append(sttsEntries, U32b(delta)...)
	stts := &Box{Type: "stts", Payload: append(FullBoxPrefix(0, 0), sttsEntries...)}

	stsc := &Box{Type: "stsc", Payload: append(FullBoxPrefix(0, 0),
		append(U32b(1), append(U32b(1),
			append(U32b(uint32(spec.VideoFrames)), U32b(1)...)...)...)...)}

	stszPayload := append(FullBoxPrefix(0, 0), U32b(0)...)
	stszPayload = append(stszPayload, U32b(uint32(spec.VideoFrames))...)
	for _, sz := range sizes {
		stszPayload = append(stszPayload, U32b(sz)...)
	}
	stsz := &Box{Type: "stsz", Payload: stszPayload}
	stco := &Box{Type: "stco", Payload: append(FullBoxPrefix(0, 0),
		append(U32b(1), U32b(0)...)...)} // 偏移占位，回填

	stblChildren := []*Box{buildSTSD(), stts, stsc, stsz, stco}
	if spec.WithSTSS {
		var syncNums []byte
		for i := 0; i < spec.VideoFrames; i += spec.IDREvery {
			syncNums = append(syncNums, U32b(uint32(i+1))...)
		}
		stss := &Box{Type: "stss", Payload: append(FullBoxPrefix(0, 0),
			append(U32b(uint32(len(syncNums)/4)), syncNums...)...)}
		stblChildren = append(stblChildren, stss)
	}
	stbl := &Box{Type: "stbl", Child: stblChildren}
	minf := &Box{Type: "minf", Child: []*Box{stbl}}

	mdhd := &Box{Type: "mdhd", Payload: append(FullBoxPrefix(0, 0),
		append(U32b(0), append(U32b(0),
			append(U32b(spec.Timescale),
				append(U32b(delta*uint32(spec.VideoFrames)), make([]byte, 4)...)...)...)...)...)}
	hdlr := &Box{Type: "hdlr", Payload: append(FullBoxPrefix(0, 0),
		append(make([]byte, 4), append([]byte("vide"),
			append(make([]byte, 12), CString(spec.Camera)...)...)...)...)}
	mdia := &Box{Type: "mdia", Child: []*Box{mdhd, hdlr, minf}}
	tkhdPayload := append(FullBoxPrefix(0, 0), make([]byte, 8)...)
	tkhdPayload = append(tkhdPayload, U32b(1)...)
	tkhdPayload = append(tkhdPayload, make([]byte, 8)...)
	tkhdPayload = append(tkhdPayload, make([]byte, 80)...)
	tkhd := &Box{Type: "tkhd", Payload: tkhdPayload}
	trak := &Box{Type: "trak", Child: []*Box{tkhd, mdia}}
	mvhd := &Box{Type: "mvhd", Payload: append(FullBoxPrefix(0, 0), make([]byte, 96)...)}
	moov := &Box{Type: "moov", Child: []*Box{mvhd, trak}}

	ftyp := &Box{Type: "ftyp", Payload: append([]byte("isom"), make([]byte, 4)...)}
	mdat := &Box{Type: "mdat", Payload: mdatPayload}

	var buf bytes.Buffer
	ftypBytes := ftyp.Encode()
	mdatBytes := mdat.Encode()
	buf.Write(ftypBytes)
	sampleBase := uint32(len(ftypBytes) + 8)
	buf.Write(mdatBytes)
	moovBytes := moov.Encode()
	// 回填 stco 首个 chunk 偏移：定位 stco 盒后的 offset 字段。
	patchSTCO(moovBytes, sampleBase)
	buf.Write(moovBytes)
	return buf.Bytes(), samples
}

func buildSTSD() *Box {
	entry := &Box{Type: "avc1", Payload: make([]byte, 78)}
	return &Box{Type: "stsd",
		Payload: append(FullBoxPrefix(0, 0), U32b(1)...), Child: []*Box{entry}}
}

// patchSTCO 找到 stco 盒并把首个 chunk offset 改成 base。
func patchSTCO(moov []byte, base uint32) {
	needle := []byte("stco")
	for i := 0; i+8 <= len(moov); i++ {
		if bytes.Equal(moov[i+4:i+8], needle) {
			// i 为盒起始：盒头8 + FullBox4 + count4 = offset 在 i+16。
			copy(moov[i+16:i+20], U32b(base))
			return
		}
	}
}

// BuildEmsg 用 go-mp4 生成确定性 emsg v1 盒字节。
func BuildEmsg(tick uint64, timescale uint32, dur, id uint32, msg string) []byte {
	src := &mp4.Emsg{
		FullBox:          mp4.FullBox{Version: 1},
		SchemeIdUri:      "urn:fieldcut:cue",
		Value:            "cam",
		Timescale:        timescale,
		PresentationTime: tick,
		EventDuration:    dur,
		Id:               id,
		MessageData:      []byte(msg),
	}
	var p bytes.Buffer
	if _, err := mp4.Marshal(&p, src, mp4.Context{}); err != nil {
		panic(err)
	}
	return (&Box{Type: "emsg", Payload: p.Bytes()}).Encode()
}
