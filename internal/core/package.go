package core

import (
	"bytes"
	"encoding/binary"
	"os"

	"mediacheck/internal/adapter"
	"mediacheck/internal/model"
)

// PackInput 是再封装输入：归一化音频 PCM 与时钟时间线。
type PackInput struct {
	OutDir        string
	BaseName      string
	Timescale     uint32
	TargetKBPS    int
	FramesPerFrag int
	PCM16         []byte
	Channels      int
	Timeline      []model.Interval
	// Seqs 是每个输出分片应使用的原始 mfhd 序号（与 Timeline 对齐）；为空时本地连续编号。
	Seqs  []uint32
	Color model.ColorMeta
}

// PackResult 是落盘结果与码率核算，供清单与报告对齐。
type PackResult struct {
	Files        []string
	MediaBytes   int64
	DurationSec  model.Rational
	ActualKBPS   int
	FreeBytes    int64
	FragmentSeqs []uint32
}

// boxWriter 生成标准 ISO box（8 字节头），供自包含 fMP4 输出使用。
func boxWriter(typ string, payload []byte) []byte {
	size := uint32(8 + len(payload))
	out := make([]byte, 8+len(payload))
	binary.BigEndian.PutUint32(out[:4], size)
	copy(out[4:8], typ)
	copy(out[8:], payload)
	return out
}

// Pack 把归一化 PCM 重新封装为固定分片大小的 fMP4（init + 若干 fragment），
// 通过 free box 填充把总码率校准到目标 ±1%；写入走临时文件，失败即清理。
func Pack(in PackInput) (*PackResult, error) {
	if in.TargetKBPS <= 0 || in.FramesPerFrag <= 0 {
		return nil, &model.MediaError{Category: model.ErrWriteFailed, Message: "invalid pack parameters"}
	}
	if err := os.MkdirAll(in.OutDir, 0o755); err != nil {
		return nil, &model.MediaError{Category: model.ErrWriteFailed, Message: err.Error()}
	}
	res := &PackResult{}
	frameBytes := in.Channels * 2 // 16-bit
	bytesPerFrag := in.FramesPerFrag * frameBytes
	totalFrames := len(in.PCM16) / frameBytes
	duration := model.NewRational(int64(totalFrames), int64(in.Timescale))
	// 目标总字节（含容器开销前的媒体目标），free box 在末尾补齐。
	targetTotal := int64(in.TargetKBPS) * 1000 / 8 * duration.Num / duration.Den

	var written []string
	cleanup := func() {
		for _, f := range written {
			_ = os.Remove(f)
		}
	}
	// init 段：ftyp + moov（极简，记录时基与声道）。
	initPath, err := adapter.SafeOutputPath(in.OutDir, in.BaseName+".mp4")
	if err != nil {
		return nil, err
	}
	initBytes := buildInit(in.Timescale, in.Channels, in.Color)
	if err := atomicWrite(initPath, initBytes); err != nil {
		return nil, err
	}
	written = append(written, initPath)
	res.Files = append(res.Files, initPath)

	// 分片写入。
	var mediaBytes int64
	for idx := 0; idx*bytesPerFrag < len(in.PCM16); idx++ {
		off := idx * bytesPerFrag
		end := off + bytesPerFrag
		if end > len(in.PCM16) {
			end = len(in.PCM16)
		}
		seq := uint32(idx + 1)
		if idx < len(in.Seqs) {
			seq = in.Seqs[idx]
		}
		baseTicks := uint32(0)
		if idx < len(in.Timeline) {
			baseTicks = uint32(in.Timeline[idx].Start.Ticks(in.Timescale))
		}
		payload := in.PCM16[off:end]
		frag := buildFragment(seq, in.Timescale, baseTicks, payload, uint32(len(payload)/frameBytes))
		fragPath, err := adapter.SafeOutputPath(in.OutDir, fragName(in.BaseName, seq))
		if err != nil {
			cleanup()
			return nil, err
		}
		if err := atomicWrite(fragPath, frag); err != nil {
			cleanup()
			return nil, err
		}
		written = append(written, fragPath)
		res.Files = append(res.Files, fragPath)
		res.FragmentSeqs = append(res.FragmentSeqs, seq)
		mediaBytes += int64(len(frag))
	}
	// free box 填充到目标总字节，保证实际码率落在目标 ±1%。
	currentTotal := mediaBytes + int64(len(initBytes))
	var freeBytes int64
	if targetTotal > currentTotal+8 {
		freeBytes = targetTotal - currentTotal
		freePayload := make([]byte, freeBytes-8) // free box 自身 8 字节头
		freePath, err := adapter.SafeOutputPath(in.OutDir, in.BaseName+".free")
		if err != nil {
			cleanup()
			return nil, err
		}
		if err := atomicWrite(freePath, boxWriter("free", freePayload)); err != nil {
			cleanup()
			return nil, err
		}
		written = append(written, freePath)
		res.Files = append(res.Files, freePath)
	}
	res.MediaBytes = mediaBytes + freeBytes
	res.FreeBytes = freeBytes
	res.DurationSec = duration
	if duration.Num > 0 {
		secs := float64(duration.Num) / float64(duration.Den)
		res.ActualKBPS = int(float64(res.MediaBytes)*8 / secs / 1000)
	}
	return res, nil
}

func fragName(base string, seq uint32) string {
	return base + "-" + padSeq(seq) + ".m4s"
}

func padSeq(seq uint32) string {
	const digits = "0123456789"
	out := make([]byte, 6)
	for i := 5; i >= 0; i-- {
		out[i] = digits[seq%10]
		seq /= 10
	}
	return string(out)
}

// atomicWrite 先写 .tmp 再 rename；任何失败删除临时产物，避免半成品覆盖。
func atomicWrite(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		_ = os.Remove(tmp)
		return &model.MediaError{Category: model.ErrWriteFailed, Message: err.Error()}
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return &model.MediaError{Category: model.ErrWriteFailed, Message: err.Error()}
	}
	return nil
}

// buildInit 生成自包含 fMP4 初始化段（ftyp+moov/trak[tkhd,mdia(mdhd,hdlr,minf/stsd(ipcm))]）。
func buildInit(timescale uint32, channels int, color model.ColorMeta) []byte {
	var b bytes.Buffer
	b.Write(boxWriter("ftyp", append([]byte("iso5"), make([]byte, 9)...)))
	// tkhd v0 字段：creation(4)+modification(4)+track_ID(4)=1
	tkhdFields := make([]byte, 12)
	binary.BigEndian.PutUint32(tkhdFields[8:12], 1)
	tkhd := fullBoxWriter("tkhd", 0x000007, tkhdFields)
	mdhdFields := make([]byte, 16)
	binary.BigEndian.PutUint32(mdhdFields[8:12], timescale)
	mdhd := fullBoxWriter("mdhd", 0, mdhdFields)
	hdlr := fullBoxWriter("hdlr", 0, append(make([]byte, 4), []byte("soun")...))
	// ipcm sample entry（与适配器解析约定一致）。
	entryBody := make([]byte, 20)
	binary.BigEndian.PutUint16(entryBody[8:10], uint16(channels))
	binary.BigEndian.PutUint16(entryBody[10:12], 16)
	binary.BigEndian.PutUint32(entryBody[16:20], timescale<<16)
	colrPayload := make([]byte, 11)
	copy(colrPayload[0:4], "nclx")
	binary.BigEndian.PutUint16(colrPayload[4:6], color.ColorPrimaries)
	binary.BigEndian.PutUint16(colrPayload[6:8], color.Transfer)
	binary.BigEndian.PutUint16(colrPayload[8:10], color.Matrix)
	entryBody = append(entryBody, boxWriter("colr", colrPayload)...)
	stsdFields := append(u32Writer(1), boxWriter("ipcm", entryBody)...)
	stsd := fullBoxWriter("stsd", 0, stsdFields)
	minf := boxWriter("minf", stsd)
	mdia := boxWriter("mdia", append(append(mdhd, hdlr...), minf...))
	trak := boxWriter("trak", append(tkhd, mdia...))
	moov := boxWriter("moov", trak)
	b.Write(moov)
	return b.Bytes()
}

// buildFragment 生成 moof(mfhd,traf(tfhd,tfdt,trun))+mdat。
func buildFragment(seq, timescale, baseTime uint32, payload []byte, sampleDur uint32) []byte {
	mfhd := fullBoxWriter("mfhd", 0, u32Writer(seq))
	tfhd := fullBoxWriter("tfhd", 0, u32Writer(1))
	tfdt := fullBoxWriter("tfdt", 0, u32Writer(baseTime))
	flags := uint32(0x000001 | 0x000100 | 0x000200) // data-offset|sample-duration|sample-size
	trunFields := append(u32Writer(1), u32Writer(0)...)
	trunFields = append(trunFields, u32Writer(sampleDur)...)
	trunFields = append(trunFields, u32Writer(uint32(len(payload)))...)
	trun := fullBoxWriter("trun", flags, trunFields)
	traf := boxWriter("traf", append(append(tfhd, tfdt...), trun...))
	moof := boxWriter("moof", append(mfhd, traf...))
	// 回填 data_offset：moof 长度 + mdat 头 8。
	dataOffPos := len(moof) - len(trun) + 8 + 4 + 4
	binary.BigEndian.PutUint32(moof[dataOffPos:dataOffPos+4], uint32(len(moof)+8))
	mdat := boxWriter("mdat", payload)
	return append(moof, mdat...)
}

func fullBoxWriter(typ string, flags uint32, fields []byte) []byte {
	body := make([]byte, 4+len(fields))
	binary.BigEndian.PutUint32(body[:4], flags)
	copy(body[4:], fields)
	return boxWriter(typ, body)
}

func u32Writer(v uint32) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, v)
	return b
}
