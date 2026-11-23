// Package fingerprint 对规范化后的整数 PCM 窗口生成确定性内容指纹，
// 识别跨块边界的重复片段并记录首次来源与时间范围。
package fingerprint

import (
	"crypto/sha256"
	"encoding/binary"
	"sort"

	"media-pipeline/internal/clock"
	"media-pipeline/internal/media"
)

// Window 是一次指纹计算的规范化窗口。
type Window struct {
	FirstSample uint64 // 窗口首帧的全局采样序号
	Frames      uint64
	SourceBlock string // 来源块标识
}

// Fingerprint 是一个窗口的内容指纹与溯源信息。
type Fingerprint struct {
	Sum         [32]byte
	FirstSample uint64
	Frames      uint64
	SourceBlock string
}

// Duplicate 记录一段重复内容：首次出现位置与再次出现的时间范围。
type Duplicate struct {
	Sum            [32]byte
	FirstSource    string // 首次来源块
	FirstSample    uint64 // 首次出现的全局采样序号
	RepeatSource   string // 重复出现的来源块
	RepeatSample   uint64 // 重复出现的全局采样序号
	Frames         uint64
}

// frameMeta 记录滚动缓冲中每一帧的来源，供跨块窗口标注溯源。
type frameMeta struct {
	block string
}

// Hasher 以固定帧窗口流式计算指纹，滚动缓冲保证跨块边界一致。
type Hasher struct {
	tb         clock.Timebase
	windowSize uint64
	frameSize  int
	buf        []byte
	bufOrigin  uint64 // buf 首帧的全局采样序号
	meta       []frameMeta

	// 首次出现索引：指纹 -> 首次窗口。
	first map[[32]byte]Fingerprint
	dups  []Duplicate
}

// New 构造指纹器，windowSize 为每个窗口的帧数（>0）。
func New(tb clock.Timebase, windowSize uint64) *Hasher {
	return &Hasher{
		tb: tb, windowSize: windowSize,
		frameSize: tb.FrameSize(),
		first:     make(map[[32]byte]Fingerprint),
	}
}

// canonical 把一个采样帧规范化为确定性字节序列：
// 统一小端、按声道固定排列，24bit 补齐为 32bit，消除位深/对齐歧义。
func canonical(raw []byte, bits, channels uint16) []byte {
	out := make([]byte, 0, int(channels)*4)
	bps := int(bits / 8)
	for c := uint16(0); c < channels; c++ {
		off := int(c) * bps
		var v int32
		switch bits {
		case 8:
			v = int32(raw[off]) - 128
		case 16:
			v = int32(int16(binary.LittleEndian.Uint16(raw[off:])))
		case 24:
			u := uint32(raw[off]) | uint32(raw[off+1])<<8 | uint32(raw[off+2])<<16
			if u&0x800000 != 0 {
				u |= 0xFF000000
			}
			v = int32(u)
		case 32:
			v = int32(binary.LittleEndian.Uint32(raw[off:]))
		}
		var b [4]byte
		binary.LittleEndian.PutUint32(b[:], uint32(v))
		out = append(out, b[:]...)
	}
	return out
}

// hashFrames 对连续的规范化帧计算 SHA-256。
func hashFrames(canon []byte) [32]byte {
	return sha256.Sum256(canon)
}

// Push 喂入一个 PCM 块，输出完整窗口的指纹，并登记重复片段。
// 不足一个窗口的尾部帧保留在滚动缓冲，与后续块拼接，
// 因此跨块边界的相同内容必然得到相同指纹。
func (h *Hasher) Push(b media.Block) ([]Fingerprint, error) {
	if len(h.buf) == 0 {
		h.bufOrigin = b.FirstSample
	}
	h.buf = append(h.buf, b.PCM...)
	frames := uint64(len(b.PCM)) / uint64(h.frameSize)
	for i := uint64(0); i < frames; i++ {
		h.meta = append(h.meta, frameMeta{block: b.ID})
	}

	var out []Fingerprint
	need := int(h.windowSize) * h.frameSize
	for len(h.buf) >= need {
		frames := h.buf[:need]
		canon := h.canonicalBytes(frames)
		sum := hashFrames(canon)
		fp := Fingerprint{
			Sum:         sum,
			FirstSample: h.bufOrigin,
			Frames:      h.windowSize,
			SourceBlock: h.meta[0].block,
		}
		h.record(fp)
		out = append(out, fp)
		h.buf = h.buf[need:] // 非重叠窗口：整窗推进，跨块拼接由滚动缓冲保证
		h.meta = h.meta[h.windowSize:]
		h.bufOrigin += h.windowSize
	}
	return out, nil
}

func (h *Hasher) canonicalBytes(frames []byte) []byte {
	var canon []byte
	for off := 0; off < len(frames); off += h.frameSize {
		canon = append(canon, canonical(frames[off:off+h.frameSize],
			h.tb.BitsPerSample, h.tb.Channels)...)
	}
	return canon
}

// record 登记指纹；若内容曾出现则生成重复记录（保留首次来源与范围）。
func (h *Hasher) record(fp Fingerprint) {
	first, seen := h.first[fp.Sum]
	if !seen {
		h.first[fp.Sum] = fp
		return
	}
	h.dups = append(h.dups, Duplicate{
		Sum:          fp.Sum,
		FirstSource:  first.SourceBlock,
		FirstSample:  first.FirstSample,
		RepeatSource: fp.SourceBlock,
		RepeatSample: fp.FirstSample,
		Frames:       fp.Frames,
	})
}

// Duplicates 返回按重复出现位置排序的重复片段。
func (h *Hasher) Duplicates() []Duplicate {
	out := append([]Duplicate(nil), h.dups...)
	sort.Slice(out, func(i, j int) bool { return out[i].RepeatSample < out[j].RepeatSample })
	return out
}

// BufferedFrames 返回滚动缓冲中滞留、尚未凑满窗口的帧数。
func (h *Hasher) BufferedFrames() uint64 {
	return uint64(len(h.buf) / h.frameSize)
}
