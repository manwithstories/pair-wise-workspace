// Package samplegen 随代码生成验收所需的确定性归档夹具和损坏样例。
package samplegen

import (
	"encoding/binary"
	"os"
	"path/filepath"
)

// AACSampleSpec 描述一个 AAC 样本的可变 stts 增量和内容种子。
type AACSampleSpec struct {
	Delta   uint32
	Payload []byte
}

// WriteAACMP4 写一个最小但可被 go-mp4 Probe 识别的单轨 AAC MP4。
// 所有样本使用固定长度，stco 指向唯一 chunk，样本在 mdat 中连续排列。
func WriteAACMP4(path string, timescale uint32, samples []AACSampleSpec) (uint32, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return 0, err
	}
	sampleSize := 24
	mdatPayload := make([]byte, 0, len(samples)*sampleSize)
	sttsEntries := make([]byte, 0, 8*len(samples))
	for i, s := range samples {
		payload := make([]byte, sampleSize)
		copy(payload, s.Payload)
		binary.BigEndian.PutUint32(payload[0:4], uint32(i+1))
		binary.BigEndian.PutUint32(payload[4:8], s.Delta)
		mdatPayload = append(mdatPayload, payload...)

		var entry [8]byte
		binary.BigEndian.PutUint32(entry[0:4], 1)
		binary.BigEndian.PutUint32(entry[4:8], s.Delta)
		sttsEntries = append(sttsEntries, entry[:]...)
	}

	ftyp := buildFtyp()
	var duration uint32
	for _, s := range samples {
		duration += s.Delta
	}
	moovPlaceholder := buildMoov(timescale, uint32(len(samples)), uint32(sampleSize), sttsEntries, 0, duration)
	dataOffset := uint32(len(ftyp) + len(moovPlaceholder) + 8)
	moov := buildMoov(timescale, uint32(len(samples)), uint32(sampleSize), sttsEntries, dataOffset, duration)

	mdatHeader := boxHeader("mdat", uint32(8+len(mdatPayload)))
	out := make([]byte, 0, len(ftyp)+len(moov)+len(mdatHeader)+len(mdatPayload))
	out = append(out, ftyp...)
	out = append(out, moov...)
	out = append(out, mdatHeader...)
	out = append(out, mdatPayload...)
	if err := os.WriteFile(path, out, 0o644); err != nil {
		return 0, err
	}
	return dataOffset, nil
}

func boxHeader(typ string, size uint32) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint32(b[0:4], size)
	copy(b[4:8], typ)
	return b
}

func box(typ string, payload []byte) []byte {
	return append(boxHeader(typ, uint32(8+len(payload))), payload...)
}

func fullBox(typ string, version byte, payload []byte) []byte {
	full := make([]byte, 4+len(payload))
	full[0] = version
	copy(full[4:], payload)
	return box(typ, full)
}

func buildFtyp() []byte {
	payload := make([]byte, 16)
	copy(payload[0:4], "isom")
	binary.BigEndian.PutUint32(payload[4:8], 0x200)
	copy(payload[8:12], "isom")
	copy(payload[12:16], "mp41")
	return box("ftyp", payload)
}

func buildMoov(timescale, sampleCount, sampleSize uint32, sttsEntries []byte, chunkOffset, duration uint32) []byte {
	mvhd := fullBox("mvhd", 0, makeMvhd(timescale, duration))
	trak := buildTrak(timescale, sampleCount, sampleSize, sttsEntries, chunkOffset, duration)
	return box("moov", append(mvhd, trak...))
}

func makeMvhd(timescale, duration uint32) []byte {
	p := make([]byte, 96)
	binary.BigEndian.PutUint32(p[8:12], timescale)
	binary.BigEndian.PutUint32(p[12:16], duration)
	binary.BigEndian.PutUint32(p[16:20], 0x00010000) // rate 1.0
	binary.BigEndian.PutUint16(p[20:22], 0x0100)     // volume full
	matrixOff := 32
	binary.BigEndian.PutUint32(p[matrixOff+0:], 0x00010000)
	binary.BigEndian.PutUint32(p[matrixOff+16:], 0x00010000)
	binary.BigEndian.PutUint32(p[matrixOff+32:], 0x40000000)
	binary.BigEndian.PutUint32(p[92:96], 2) // next track ID
	return p
}

func buildTrak(timescale, sampleCount, sampleSize uint32, sttsEntries []byte, chunkOffset, duration uint32) []byte {
	tkhd := fullBox("tkhd", 0, makeTkhd())
	mdhd := fullBox("mdhd", 0, makeMdhd(timescale, duration))
	hdlr := buildHdlr()
	stsd := buildStsd()
	stts := fullBox("stts", 0, append(u32Bytes(uint32(len(sttsEntries)/8)), sttsEntries...))
	stscPayload := append(u32Bytes(1), u32Bytes(sampleCount)...)
	stscPayload = append(stscPayload, u32Bytes(1)...)
	stsc := fullBox("stsc", 0, append(u32Bytes(1), stscPayload...))
	stszPayload := append(u32Bytes(sampleSize), u32Bytes(sampleCount)...)
	stsz := fullBox("stsz", 0, stszPayload)
	stco := fullBox("stco", 0, append(u32Bytes(1), u32Bytes(chunkOffset)...))

	stbl := box("stbl", appendMany(stsd, stts, stsc, stsz, stco))
	minf := box("minf", stbl)
	mdia := box("mdia", appendMany(mdhd, hdlr, minf))
	return box("trak", append(tkhd, mdia...))
}

func makeTkhd() []byte {
	p := make([]byte, 80)
	binary.BigEndian.PutUint32(p[8:12], 1)  // track ID
	binary.BigEndian.PutUint32(p[12:16], 0) // reserved
	binary.BigEndian.PutUint32(p[16:20], 0) // duration unknown but valid
	binary.BigEndian.PutUint16(p[32:34], 0) // layer
	binary.BigEndian.PutUint16(p[34:36], 0) // alternate group
	binary.BigEndian.PutUint16(p[36:38], 0x0100)
	matrixOff := 36
	binary.BigEndian.PutUint32(p[matrixOff+0:], 0x00010000)
	binary.BigEndian.PutUint32(p[matrixOff+16:], 0x00010000)
	binary.BigEndian.PutUint32(p[matrixOff+32:], 0x40000000)
	binary.BigEndian.PutUint32(p[72:76], 320<<16)
	binary.BigEndian.PutUint32(p[76:80], 180<<16)
	return p
}

func makeMdhd(timescale, duration uint32) []byte {
	p := make([]byte, 20)
	binary.BigEndian.PutUint32(p[8:12], timescale)
	binary.BigEndian.PutUint32(p[12:16], duration)
	binary.BigEndian.PutUint16(p[16:18], 0x55c4) // language "und"
	return p
}

func buildHdlr() []byte {
	p := make([]byte, 21)
	copy(p[4:8], "soun")
	p[20] = 0
	return fullBox("hdlr", 0, p)
}

func buildStsd() []byte {
	entry := make([]byte, 28)
	binary.BigEndian.PutUint16(entry[6:8], 1)   // data reference index
	binary.BigEndian.PutUint16(entry[16:18], 2) // channels
	binary.BigEndian.PutUint16(entry[18:20], 16)
	binary.BigEndian.PutUint16(entry[20:22], 0)
	binary.BigEndian.PutUint32(entry[24:28], 48000<<16)
	mp4a := box("mp4a", entry)
	return fullBox("stsd", 0, append(u32Bytes(1), mp4a...))
}

// WriteTruncatedBoxSample 写一个 ftyp 后紧跟声明超长但实际截断的 stts box。
// 返回结构 box 头的精确偏移，供验收 offset_error=0 比对。
func WriteTruncatedBoxSample(path string, packetNo int) (int64, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return 0, err
	}
	ftyp := buildFtyp()
	offset := int64(len(ftyp))
	header := boxHeader("stts", 192) // 声明 192 字节
	actualPayload := make([]byte, 24)
	var marker [8]byte
	binary.BigEndian.PutUint32(marker[0:4], uint32(packetNo))
	copy(actualPayload[:8], marker[:])
	out := append(append(ftyp, header...), actualPayload...)
	if err := os.WriteFile(path, out, 0o644); err != nil {
		return 0, err
	}
	return offset, nil
}

func u32Bytes(v uint32) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, v)
	return b
}

func appendMany(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}
